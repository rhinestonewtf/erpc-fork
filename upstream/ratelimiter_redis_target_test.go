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
	type want struct {
		host     string
		user     string
		pass     string
		path     string
		rawQuery string
		tls      bool
	}
	tests := []struct {
		name    string
		cfg     *common.RedisConnectorConfig
		want    want
		wantErr string
	}{
		{
			// The shape our ElastiCache module emits: ACL user, url-encoded password, TLS.
			name: "rediss uri with encoded credentials",
			cfg:  &common.RedisConnectorConfig{URI: "rediss://redis-admin:p%40ss%3Aword@host.cache.amazonaws.com:6379"},
			want: want{host: "host.cache.amazonaws.com:6379", user: "redis-admin", pass: "p@ss:word", tls: true},
		},
		{
			// What RedisConnectorConfig.SetDefaults builds from discrete fields.
			name: "rediss uri with db 0 path",
			cfg:  &common.RedisConnectorConfig{URI: "rediss://u:p@host:6379/0"},
			want: want{host: "host:6379", user: "u", pass: "p", path: "/0", tls: true},
		},
		{
			// Non-zero DB must survive: radix SELECTs it from the URL path.
			name: "rediss uri with db path",
			cfg:  &common.RedisConnectorConfig{URI: "rediss://u:p@host:6379/2"},
			want: want{host: "host:6379", user: "u", pass: "p", path: "/2", tls: true},
		},
		{
			name: "redis uri with db query",
			cfg:  &common.RedisConnectorConfig{URI: "redis://host:6379?db=3"},
			want: want{host: "host:6379", rawQuery: "db=3"},
		},
		{
			name: "redis uri without credentials",
			cfg:  &common.RedisConnectorConfig{URI: "redis://localhost:6380"},
			want: want{host: "localhost:6380"},
		},
		{
			name: "redis uri password only",
			cfg:  &common.RedisConnectorConfig{URI: "redis://:secret@localhost:6379"},
			want: want{host: "localhost:6379", pass: "secret"},
		},
		{
			name: "uri without port defaults to 6379",
			cfg:  &common.RedisConnectorConfig{URI: "rediss://host"},
			want: want{host: "host:6379", tls: true},
		},
		{
			name: "tls.enabled forces TLS on a plain redis uri",
			cfg:  &common.RedisConnectorConfig{URI: "redis://host:6379", TLS: &common.TLSConfig{Enabled: true}},
			want: want{host: "host:6379", tls: true},
		},
		{
			name: "bare addr with discrete fields and db",
			cfg:  &common.RedisConnectorConfig{Addr: "host:6379", Username: "u", Password: "p", DB: 3, TLS: &common.TLSConfig{Enabled: true}},
			want: want{host: "host:6379", user: "u", pass: "p", path: "/3", tls: true},
		},
		{
			name: "bare addr plaintext",
			cfg:  &common.RedisConnectorConfig{Addr: "host:6379"},
			want: want{host: "host:6379"},
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

			u, err := url.Parse(got.Addr)
			require.NoError(t, err)
			// radix v3 only parses this scheme; TLS travels separately.
			assert.Equal(t, "redis", u.Scheme)
			assert.Equal(t, tc.want.host, u.Host)
			assert.Equal(t, tc.want.user, u.User.Username())
			pass, _ := u.User.Password()
			assert.Equal(t, tc.want.pass, pass)
			assert.Equal(t, tc.want.path, u.Path)
			assert.Equal(t, tc.want.rawQuery, u.RawQuery)

			assert.Equal(t, tc.want.tls, got.UseTLS)
			if tc.want.tls {
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
// the fork patch: a rediss:// URI with ACL credentials and a non-default DB must
// produce a store that actually enforces, in that DB. Without the patch envoy
// dials the URI as a TCP address, the connect fails, getCache() stays nil and
// every permit is granted, so the Eventually below times out.
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
		Path:   "/2",
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
	assert.NotEmpty(t, srv.DB(2).Keys(), "counters must live in the DB named by the URI")
	assert.Empty(t, srv.DB(0).Keys(), "nothing may leak into DB 0")
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
