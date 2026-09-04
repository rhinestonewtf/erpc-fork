package upstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/erpc/erpc/common"
)

func TestResolveRateLimiterRedisTarget(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *common.RedisConnectorConfig
		wantAddr string
		wantAuth string
		wantTLS  bool
		wantErr  string
	}{
		{
			// The shape our ElastiCache module emits: ACL user, url-encoded password, TLS.
			name:     "rediss uri with encoded credentials",
			cfg:      &common.RedisConnectorConfig{URI: "rediss://redis-admin:p%40ss%3Aword@host.cache.amazonaws.com:6379"},
			wantAddr: "host.cache.amazonaws.com:6379",
			wantAuth: "redis-admin:p@ss:word",
			wantTLS:  true,
		},
		{
			// What RedisConnectorConfig.SetDefaults builds from discrete fields.
			name:     "rediss uri with db 0 path",
			cfg:      &common.RedisConnectorConfig{URI: "rediss://u:p@host:6379/0"},
			wantAddr: "host:6379",
			wantAuth: "u:p",
			wantTLS:  true,
		},
		{
			name:     "redis uri without credentials",
			cfg:      &common.RedisConnectorConfig{URI: "redis://localhost:6380"},
			wantAddr: "localhost:6380",
		},
		{
			name:     "redis uri password only",
			cfg:      &common.RedisConnectorConfig{URI: "redis://:secret@localhost:6379"},
			wantAddr: "localhost:6379",
			wantAuth: "secret",
		},
		{
			name:     "uri without port defaults to 6379",
			cfg:      &common.RedisConnectorConfig{URI: "rediss://host"},
			wantAddr: "host:6379",
			wantTLS:  true,
		},
		{
			name:     "tls.enabled forces TLS on a plain redis uri",
			cfg:      &common.RedisConnectorConfig{URI: "redis://host:6379", TLS: &common.TLSConfig{Enabled: true}},
			wantAddr: "host:6379",
			wantTLS:  true,
		},
		{
			name:     "bare addr with discrete fields",
			cfg:      &common.RedisConnectorConfig{Addr: "host:6379", Username: "u", Password: "p", TLS: &common.TLSConfig{Enabled: true}},
			wantAddr: "host:6379",
			wantAuth: "u:p",
			wantTLS:  true,
		},
		{
			name:     "bare addr plaintext",
			cfg:      &common.RedisConnectorConfig{Addr: "host:6379"},
			wantAddr: "host:6379",
		},
		{
			name:    "non-zero db in uri is refused",
			cfg:     &common.RedisConnectorConfig{URI: "redis://host:6379/2"},
			wantErr: "only db 0",
		},
		{
			name:    "non-zero db field is refused",
			cfg:     &common.RedisConnectorConfig{Addr: "host:6379", DB: 3},
			wantErr: "only db 0",
		},
		{
			name:    "unsupported scheme",
			cfg:     &common.RedisConnectorConfig{URI: "http://host:6379"},
			wantErr: "unsupported scheme",
		},
		{
			name:    "empty",
			cfg:     &common.RedisConnectorConfig{},
			wantErr: "neither uri nor addr",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRateLimiterRedisTarget(tc.cfg)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAddr, got.Addr)
			assert.Equal(t, tc.wantAuth, got.Auth)
			assert.Equal(t, tc.wantTLS, got.UseTLS)
			if tc.wantTLS {
				require.NotNil(t, got.TLSConfig, "TLS on needs a config for envoy's dialer")
			} else {
				assert.Nil(t, got.TLSConfig)
			}
		})
	}

	t.Run("tls block is honoured alongside a rediss uri", func(t *testing.T) {
		got, err := resolveRateLimiterRedisTarget(&common.RedisConnectorConfig{
			URI: "rediss://host:6379",
			TLS: &common.TLSConfig{InsecureSkipVerify: true},
		})
		require.NoError(t, err)
		assert.True(t, got.UseTLS)
		require.NotNil(t, got.TLSConfig)
		assert.True(t, got.TLSConfig.InsecureSkipVerify)
	})
}

// TestRateLimitersRegistry_RedissURI_SharedCounter is the behavioural guard for
// the fork patch: a rediss:// URI with ACL credentials must produce a store
// that actually enforces. Without the patch envoy dials the URI as a TCP
// address, the connect fails, getCache() stays nil and every permit is
// granted, so the Eventually below times out.
func TestRateLimitersRegistry_RedissURI_SharedCounter(t *testing.T) {
	srv, err := miniredis.RunTLS(&tls.Config{
		Certificates: []tls.Certificate{selfSignedTestCert(t)},
		MinVersion:   tls.VersionTLS12,
	})
	require.NoError(t, err)
	defer srv.Close()
	srv.RequireUserAuth("limiter", "s3cr:et")

	uri := url.URL{
		Scheme: "rediss",
		User:   url.UserPassword("limiter", "s3cr:et"),
		Host:   srv.Addr(),
	}
	cfg := &common.RateLimiterConfig{
		Store: &common.RateLimitStoreConfig{
			Driver: "redis",
			Redis: &common.RedisConnectorConfig{
				URI: uri.String(),
				TLS: &common.TLSConfig{InsecureSkipVerify: true},
			},
		},
		Budgets: []*common.RateLimitBudgetConfig{{
			Id: "shared",
			Rules: []*common.RateLimitRuleConfig{{
				Method:   "*",
				MaxCount: 3,
				Period:   common.RateLimitPeriodMinute,
			}},
		}},
	}
	require.NoError(t, cfg.SetDefaults())

	logger := zerolog.Nop()
	registry, err := NewRateLimitersRegistry(context.Background(), cfg, &logger)
	require.NoError(t, err)
	budget, err := registry.GetBudget("shared")
	require.NoError(t, err)

	require.Eventually(t, func() bool { return budget.getCache() != nil },
		5*time.Second, 50*time.Millisecond,
		"rate limiter never connected to the rediss:// store, so it is fail-open")

	allowed := 0
	for i := 0; i < 4; i++ {
		ok, err := budget.TryAcquirePermit(context.Background(), "p", nil, "eth_call", "", "", "", "")
		require.NoError(t, err)
		if ok {
			allowed++
		}
	}
	assert.Equal(t, 3, allowed, "4th permit in the window must be refused by the remote counter")
	assert.NotEmpty(t, srv.Keys(), "counters must live in Redis, not in-process")
}

func selfSignedTestCert(t *testing.T) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "miniredis"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
