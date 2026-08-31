// Package ratelimit implements a built-in rate limiting plugin for the aware-gateway.
//
// It provides global and per-key rate limiting using golang.org/x/time/rate
// (token bucket algorithm). Per-key limiting reads a configurable header
// (default: X-Heidi-Key) to identify the rate limit key.
//
// Configuration (under plugins.ratelimit in gateway.yaml):
//
//	plugins:
//	  ratelimit:
//	    enabled: true
//	    global_rps: 100
//	    per_key_rps: 20
//	    key_header: "X-Heidi-Key"
package ratelimit

import (
	"log/slog"
	"net/http"
	"sync"

	"golang.org/x/time/rate"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/plugin"
)

type Config struct {
	Enabled    bool    `yaml:"enabled"`
	GlobalRPS  float64 `yaml:"global_rps"`
	PerKeyRPS  float64 `yaml:"per_key_rps"`
	KeyHeader  string  `yaml:"key_header"`
}

type Plugin struct {
	cfg          Config
	logger       *slog.Logger
	globalLimiter *rate.Limiter

	mu       sync.Mutex
	keyLimiters map[string]*rate.Limiter
}

func (p *Plugin) Name() string { return "ratelimit" }

func (p *Plugin) Init(ctx *plugin.Context) error {
	p.logger = ctx.Logger

	cfg, ok := config.PluginConfig[Config](ctx.Config, "ratelimit")
	if !ok {
		// Fall back to top-level config
		cfg = Config{
			Enabled:   ctx.Config.RateLimit.GlobalRPS > 0,
			GlobalRPS: ctx.Config.RateLimit.GlobalRPS,
			PerKeyRPS: ctx.Config.RateLimit.PerKeyRPS,
			KeyHeader: ctx.Config.RateLimit.KeyHeader,
		}
	}
	p.cfg = cfg

	if !cfg.Enabled {
		p.logger.Info("ratelimit: disabled")
		return nil
	}

	if cfg.GlobalRPS <= 0 {
		cfg.GlobalRPS = 100
	}
	p.globalLimiter = newLimiter(cfg.GlobalRPS)

	if cfg.PerKeyRPS > 0 && cfg.KeyHeader == "" {
		cfg.KeyHeader = "X-Heidi-Key"
	}
	p.keyLimiters = make(map[string]*rate.Limiter)

	p.logger.Info("ratelimit initialized",
		"global_rps", cfg.GlobalRPS,
		"per_key_rps", cfg.PerKeyRPS,
		"key_header", cfg.KeyHeader,
	)
	return nil
}

func (p *Plugin) Close() error { return nil }

func (p *Plugin) Middleware() func(http.Handler) http.Handler {
	if !p.cfg.Enabled {
		return nil
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Global limit
			if !p.globalLimiter.Allow() {
				slog.Warn("rate limited (global)",
					"method", r.Method,
					"path", r.URL.Path,
					"remote", r.RemoteAddr,
				)
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}

			// Per-key limit
			if p.cfg.PerKeyRPS > 0 && p.cfg.KeyHeader != "" {
				key := r.Header.Get(p.cfg.KeyHeader)
				if key != "" {
					limiter := p.getLimiter(key)
					if !limiter.Allow() {
						slog.Warn("rate limited (per-key)",
							"header", p.cfg.KeyHeader,
							"key", key,
							"method", r.Method,
							"path", r.URL.Path,
						)
						http.Error(w, "too many requests", http.StatusTooManyRequests)
						return
					}
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

func (p *Plugin) getLimiter(key string) *rate.Limiter {
	p.mu.Lock()
	defer p.mu.Unlock()
	if l, ok := p.keyLimiters[key]; ok {
		return l
	}
	l := newLimiter(p.cfg.PerKeyRPS)
	p.keyLimiters[key] = l
	return l
}

func newLimiter(rps float64) *rate.Limiter {
	burst := int(rps * 2)
	if burst < 1 {
		burst = 1
	}
	return rate.NewLimiter(rate.Limit(rps), burst)
}
