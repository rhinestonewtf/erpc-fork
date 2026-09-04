package upstream

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/erpc/erpc/common"
)

// rateLimiterRedisTarget is what envoyproxy/ratelimit's NewClientImpl actually
// takes: a bare host:port, an optional "user:pass" auth string, and TLS as a
// flag plus config. It has never accepted a URI (its own settings are
// REDIS_URL, REDIS_AUTH and REDIS_TLS, separately), and radix v3, which it
// dials through, only recognises the redis:// scheme and never turns TLS on
// from a URL. Upstream eRPC hands Store.Redis.URI to it verbatim, so a
// rediss://user:pass@host:port URI is used as a TCP address, the dial fails,
// and every budget on the store fail-opens for the life of the process, with
// nothing logged above Warn.
//
// Fork patch (RHI-6529): resolve the connector config into those pieces here,
// so `uri: rediss://...` works for the rate limiter the way it already does
// for the cache and shared-state connectors (which parse it via go-redis).
type rateLimiterRedisTarget struct {
	Addr      string
	Auth      string
	UseTLS    bool
	TLSConfig *tls.Config
}

func resolveRateLimiterRedisTarget(cfg *common.RedisConnectorConfig) (*rateLimiterRedisTarget, error) {
	if cfg == nil {
		return nil, fmt.Errorf("rate-limiter redis: nil connector config")
	}
	raw := cfg.URI
	if raw == "" {
		raw = cfg.Addr
	}
	if raw == "" {
		return nil, fmt.Errorf("rate-limiter redis: neither uri nor addr is set")
	}

	t := &rateLimiterRedisTarget{
		UseTLS: cfg.TLS != nil && cfg.TLS.Enabled,
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("rate-limiter redis: parse uri: %w", err)
		}
		switch u.Scheme {
		case "redis":
		case "rediss":
			t.UseTLS = true
		default:
			return nil, fmt.Errorf("rate-limiter redis: unsupported scheme %q (want redis:// or rediss://)", u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("rate-limiter redis: uri has no host")
		}
		t.Addr = withDefaultRedisPort(u.Host)
		if u.User != nil {
			// Password() percent-decodes, which is what both our SetDefaults
			// (url.UserPassword) and any URI-producing tool apply on the way in.
			pass, _ := u.User.Password()
			t.Auth = joinRedisAuth(u.User.Username(), pass)
		}
		// envoy's client has no SELECT hook, so a non-default DB would be
		// silently ignored and counters would land in db 0 anyway. Refuse
		// rather than count against a keyspace the operator did not name.
		if db := strings.TrimPrefix(u.Path, "/"); db != "" && db != "0" {
			return nil, fmt.Errorf("rate-limiter redis: db %q in uri is not supported by the rate limiter store (only db 0)", db)
		}
	} else {
		if cfg.DB != 0 {
			return nil, fmt.Errorf("rate-limiter redis: db %d is not supported by the rate limiter store (only db 0)", cfg.DB)
		}
		t.Addr = withDefaultRedisPort(raw)
		t.Auth = joinRedisAuth(cfg.Username, cfg.Password)
	}

	if t.UseTLS {
		if cfg.TLS != nil {
			// Honour insecureSkipVerify / caFile / client certs when given,
			// exactly as the IAM path and the cache connector do.
			tlsCfg, err := common.CreateTLSConfig(cfg.TLS)
			if err != nil {
				return nil, fmt.Errorf("rate-limiter redis: tls config: %w", err)
			}
			t.TLSConfig = tlsCfg
		} else {
			// System roots; ServerName is filled from Addr by crypto/tls.
			t.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
	}

	return t, nil
}

// joinRedisAuth renders credentials in the form envoy's dialer expects:
// "user:pass" selects AUTH <user> <pass>, a bare string selects AUTH <pass>.
func joinRedisAuth(user, pass string) string {
	if user != "" {
		return user + ":" + pass
	}
	return pass
}

func withDefaultRedisPort(hostport string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(hostport, "6379")
}
