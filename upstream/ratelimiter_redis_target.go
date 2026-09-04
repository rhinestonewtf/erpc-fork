package upstream

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
)

// rateLimiterRedisTarget is what envoyproxy/ratelimit's NewClientImpl actually
// takes: an address radix can dial, plus TLS as a flag and config. radix v3
// accepts a redis:// URL as the address and applies the credentials and the
// /N (or ?db=N) database from it itself, but it only recognises the redis://
// scheme and never turns TLS on from a URL. Upstream eRPC hands
// Store.Redis.URI through verbatim, so a rediss://user:pass@host:port URI is
// used as a raw TCP address ("too many colons in address"), the dial fails,
// and every budget on the store fail-opens for the life of the process, with
// nothing logged above Warn.
//
// Fork patch (RHI-6529): normalise the connector config into a redis:// URL
// so radix keeps doing AUTH/SELECT exactly as before, and lift TLS out of the
// scheme into envoy's dialer flag. `uri: rediss://...` then works for the rate
// limiter the way it already does for the cache and shared-state connectors.
type rateLimiterRedisTarget struct {
	// Addr is a redis:// URL (never rediss://). Credentials and DB stay in it
	// for radix; envoy's separate auth argument must remain empty or it would
	// override them.
	Addr      string
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

	var u *url.URL
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("rate-limiter redis: parse uri: %w", err)
		}
		switch parsed.Scheme {
		case "redis":
		case "rediss":
			t.UseTLS = true
		default:
			return nil, fmt.Errorf("rate-limiter redis: unsupported scheme %q (want redis:// or rediss://)", parsed.Scheme)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("rate-limiter redis: uri has no host")
		}
		u = parsed
		u.Scheme = "redis"
		u.Host = withDefaultRedisPort(u.Host)
	} else {
		// A bare host:port normally never reaches here (SetDefaults rewrites
		// the discrete fields into a URI), but keep it equivalent: carry the
		// credentials and DB in the URL so radix applies them the same way.
		u = &url.URL{Scheme: "redis", Host: withDefaultRedisPort(raw)}
		if cfg.Username != "" || cfg.Password != "" {
			u.User = url.UserPassword(cfg.Username, cfg.Password)
		}
		if cfg.DB != 0 {
			u.Path = "/" + strconv.Itoa(cfg.DB)
		}
	}
	t.Addr = u.String()

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
			// System roots; ServerName is filled from the host by crypto/tls.
			t.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
	}

	return t, nil
}

func withDefaultRedisPort(hostport string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(hostport, "6379")
}
