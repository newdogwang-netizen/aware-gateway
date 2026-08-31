package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/metrics"
	"github.com/aware/gateway/internal/plugin"
	"github.com/aware/gateway/internal/pool"
)

// PoolManager provides atomic pool swapping for hot reload.
type PoolManager struct {
	mu         sync.RWMutex
	pools      map[string]*pool.Pool
	currentCfg *config.Config
}

// SwapAndReturn replaces the current pools and returns the old ones.
// The caller is responsible for stopping health checks on the old pools.
func (pm *PoolManager) SwapAndReturn(newPools map[string]*pool.Pool, newCfg *config.Config) map[string]*pool.Pool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	old := pm.pools
	pm.pools = newPools
	pm.currentCfg = newCfg
	return old
}

func (pm *PoolManager) Get(name string) (*pool.Pool, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	p, ok := pm.pools[name]
	return p, ok
}

func (pm *PoolManager) All() map[string]*pool.Pool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make(map[string]*pool.Pool, len(pm.pools))
	for k, v := range pm.pools {
		out[k] = v
	}
	return out
}

func (pm *PoolManager) Config() *config.Config {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.currentCfg
}

func NewPoolManager() *PoolManager {
	return &PoolManager{}
}

// BuildRouter creates the chi router with all middleware, routes, and the
// core handler. Plugin middleware is wrapped around the core handler.
func BuildRouter(
	cfg *config.Config,
	pp PoolProvider,
	reg *plugin.Registry,
	logger *slog.Logger,
) http.Handler {
	r := chi.NewRouter()

	// chi built-in middleware
	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.RealIP)
	r.Use(chiMiddleware.Recoverer)
	r.Use(chiMiddleware.Logger)

	// Body size limit
	r.Use(maxBodyReader(maxBodySize))

	// OpenTelemetry
	if cfg.Tracing.Enabled {
		r.Use(otelhttp.NewMiddleware(cfg.Tracing.ServiceName))
	}

	// Plugin-provided middleware (wrapped in priority order)
	for _, mp := range reg.MiddlewareProviders() {
		mw := mp.Middleware()
		if mw != nil {
			r.Use(mw)
			logger.Info("plugin middleware registered", "plugin", mp.Name())
		}
	}

	// Core handler for proxied routes
	handler := NewHandler(cfg, pp, reg, logger)

	// Static endpoints
	r.Get("/health", healthHandler(pp, reg))
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Get("/v1/usage", usageHandler(reg))

	// Plugin health reporters endpoint
	r.Get("/v1/plugins", pluginsHandler(reg))

	// Register proxied routes
	for _, route := range cfg.Routes {
		if _, ok := pp.Get(route.Pool); !ok {
			logger.Error("route references unknown pool",
				"pattern", route.Pattern,
				"pool", route.Pool,
			)
			continue
		}
		r.Handle(route.Pattern, handler)
		logger.Info("route registered",
			"pattern", route.Pattern,
			"pool", route.Pool,
		)
	}

	return r
}

// CreatePools builds pool objects from config and starts health checks.
func CreatePools(cfg *config.Config) map[string]*pool.Pool {
	ctx := context.Background()
	pools := make(map[string]*pool.Pool, len(cfg.Pools))
	for name, poolCfg := range cfg.Pools {
		p, err := pool.NewPool(name, poolCfg, cfg.CircuitBreaker)
		if err != nil {
			slog.Error("failed to create pool", "pool", name, "error", err)
			continue
		}
		pools[name] = p
		p.StartHealthChecks(ctx)
		slog.Info("pool initialized",
			"pool", name,
			"endpoints", len(p.Endpoints),
			"strategy", poolCfg.Strategy,
		)
	}
	return pools
}

// RefreshPoolGauges updates Prometheus pool health gauges periodically.
func RefreshPoolGauges(ctx context.Context, pp PoolProvider) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pools := pp.All()
			for name, p := range pools {
				ps := p.GetStatus()
				upCount := 0
				for _, ep := range ps.Endpoints {
					if ep.Healthy {
						upCount++
					}
					metrics.CircuitBreakerState.WithLabelValues(name, ep.Name).Set(
						metrics.BreakerState(ep.BreakerState),
					)
				}
				metrics.PoolEndpointsUp.WithLabelValues(name).Set(float64(upCount))
				metrics.PoolEndpointsTotal.WithLabelValues(name).Set(float64(len(ps.Endpoints)))
			}
		}
	}
}

// --- HTTP Handlers ---

func healthHandler(pp PoolProvider, reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		pools := pp.All()
		allHealthy := true
		poolStatuses := make(map[string]pool.PoolStatus, len(pools))
		for name, p := range pools {
			ps := p.GetStatus()
			poolStatuses[name] = ps
			for _, ep := range ps.Endpoints {
				if !ep.Healthy {
					allHealthy = false
				}
				metrics.CircuitBreakerState.WithLabelValues(name, ep.Name).Set(
					metrics.BreakerState(ep.BreakerState),
				)
			}
			metrics.PoolEndpointsUp.WithLabelValues(name).Set(float64(countHealthy(ps)))
			metrics.PoolEndpointsTotal.WithLabelValues(name).Set(float64(len(ps.Endpoints)))
		}

		status := "ok"
		if !allHealthy {
			status = "degraded"
		}

		// Collect plugin health reports
		pluginStatuses := make(map[string]interface{}, 0)
		for _, hr := range reg.HealthReporters() {
			pluginStatuses[hr.Name()] = hr.Status()
		}

		resp := struct {
			Status  string                   `json:"status"`
			Pools   map[string]pool.PoolStatus `json:"pools"`
			Plugins map[string]interface{}   `json:"plugins,omitempty"`
		}{
			Status:  status,
			Pools:   poolStatuses,
			Plugins: pluginStatuses,
		}

		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(resp)
	}
}

func usageHandler(reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Delegate to audit sink plugins that support querying
		// For now, return plugin list
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"plugins": pluginNames(reg),
		})
	}
}

func pluginsHandler(reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var plugins []map[string]interface{}
		for _, p := range reg.AllPlugins() {
			info := map[string]interface{}{
				"name": p.Name(),
			}
			if hr, ok := p.(plugin.HealthReporter); ok {
				info["status"] = hr.Status()
			}
			plugins = append(plugins, info)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"plugins": plugins,
		})
	}
}

func pluginNames(reg *plugin.Registry) []string {
	var names []string
	for _, p := range reg.AllPlugins() {
		names = append(names, p.Name())
	}
	return names
}

func countHealthy(ps pool.PoolStatus) int {
	n := 0
	for _, ep := range ps.Endpoints {
		if ep.Healthy {
			n++
		}
	}
	return n
}

func maxBodyReader(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// (removed unused import guards)
