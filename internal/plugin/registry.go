package plugin

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// Registry manages plugin lifecycle: registration, initialization, and shutdown.
// It is safe for concurrent use after Init() completes.
type Registry struct {
	mu      sync.RWMutex
	plugins []Plugin
	ctx     *Context
	logger  *slog.Logger

	// Categorized hooks (populated in Init)
	routers             []RequestRouter
	reqTransformers     []RequestTransformer
	respTransformers    []ResponseTransformer
	authenticators      []Authenticator
	auditSinks          []AuditSink
	middlewareProviders []MiddlewareProvider
	healthReporters     []HealthReporter
}

// NewRegistry creates an empty plugin registry.
func NewRegistry(logger *slog.Logger) *Registry {
	return &Registry{logger: logger}
}

// Register adds a plugin to the registry. Must be called before Init().
// Duplicate names are rejected.
func (r *Registry) Register(p Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := p.Name()
	for _, existing := range r.plugins {
		if existing.Name() == name {
			return fmt.Errorf("plugin %q already registered", name)
		}
	}
	r.plugins = append(r.plugins, p)
	return nil
}

// Init initializes all registered plugins in priority order.
// It also categorizes plugins by hook interface for fast dispatch.
func (r *Registry) Init(ctx *Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ctx = ctx

	// Sort by priority (lower = earlier)
	sort.SliceStable(r.plugins, func(i, j int) bool {
		return getPriority(r.plugins[i]) < getPriority(r.plugins[j])
	})

	// Initialize and categorize
	for _, p := range r.plugins {
		if err := p.Init(ctx); err != nil {
			return fmt.Errorf("plugin %q init failed: %w", p.Name(), err)
		}
		r.logger.Info("plugin initialized", "name", p.Name())

		// Categorize by interface
		if rr, ok := p.(RequestRouter); ok {
			r.routers = append(r.routers, rr)
		}
		if rt, ok := p.(RequestTransformer); ok {
			r.reqTransformers = append(r.reqTransformers, rt)
		}
		if st, ok := p.(ResponseTransformer); ok {
			r.respTransformers = append(r.respTransformers, st)
		}
		if au, ok := p.(Authenticator); ok {
			r.authenticators = append(r.authenticators, au)
		}
		if as, ok := p.(AuditSink); ok {
			r.auditSinks = append(r.auditSinks, as)
		}
		if mp, ok := p.(MiddlewareProvider); ok {
			r.middlewareProviders = append(r.middlewareProviders, mp)
		}
		if hr, ok := p.(HealthReporter); ok {
			r.healthReporters = append(r.healthReporters, hr)
		}
	}

	r.logger.Info("plugins initialized",
		"total", len(r.plugins),
		"routers", len(r.routers),
		"req_transformers", len(r.reqTransformers),
		"resp_transformers", len(r.respTransformers),
		"authenticators", len(r.authenticators),
		"audit_sinks", len(r.auditSinks),
		"middleware", len(r.middlewareProviders),
		"health_reporters", len(r.healthReporters),
	)
	return nil
}

// Close shuts down all plugins in reverse priority order.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var firstErr error
	for i := len(r.plugins) - 1; i >= 0; i-- {
		p := r.plugins[i]
		if err := p.Close(); err != nil {
			r.logger.Error("plugin close error", "name", p.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// --- Hook accessors (used by core engine) ---

func (r *Registry) Routers() []RequestRouter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.routers
}

func (r *Registry) RequestTransformers() []RequestTransformer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reqTransformers
}

func (r *Registry) ResponseTransformers() []ResponseTransformer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.respTransformers
}

func (r *Registry) Authenticators() []Authenticator {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.authenticators
}

func (r *Registry) AuditSinks() []AuditSink {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.auditSinks
}

func (r *Registry) MiddlewareProviders() []MiddlewareProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.middlewareProviders
}

func (r *Registry) HealthReporters() []HealthReporter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.healthReporters
}

// AllPlugins returns all registered plugins (for debugging/inspection).
func (r *Registry) AllPlugins() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Plugin, len(r.plugins))
	copy(out, r.plugins)
	return out
}

// --- Helpers ---

func getPriority(p Plugin) int {
	if pp, ok := p.(PriorityPlugin); ok {
		return pp.Priority()
	}
	return 100
}
