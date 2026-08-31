// Package plugin defines the extension interfaces for the aware-gateway.
//
// The gateway is built as a core proxy engine with a plugin system. All
// non-core behavior — routing decisions, authentication, audit sinks,
// request/response transformation, custom middleware — is implemented
// as plugins that implement one or more hook interfaces.
//
// Plugin Lifecycle:
//  1. Register: plugins are registered with the Registry before startup
//  2. Init: Registry.Init() calls Plugin.Init() for each plugin with a Context
//  3. Serve: hook methods are called per-request in priority order
//  4. Close: Registry.Close() calls Plugin.Close() on graceful shutdown
//
// Hook Execution:
//  - RequestRouter: chain-of-responsibility — first non-Skip decision wins
//  - RequestTransformer: pipeline — each transformer runs in order
//  - ResponseTransformer: pipeline — each transformer runs in order
//  - Authenticator: all must pass (AND logic)
//  - AuditSink: fan-out — each sink receives every event
//  - MiddlewareProvider: middleware wrapped in registration order
package plugin

import (
	"net/http"
)

// Plugin is the base interface. Every extension must implement this.
// The Name() must be unique across all registered plugins.
type Plugin interface {
	Name() string
	Init(ctx *Context) error
	Close() error
}

// Priority controls hook execution order. Lower values run first.
// Plugins with the same priority run in registration order.
// Default priority is 100 if PriorityPlugin is not implemented.
type PriorityPlugin interface {
	Priority() int
}

// RequestRouter decides which pool and model to route a request to.
// Called before pool selection. Multiple routers form a chain:
// the first router that returns a non-Skip decision wins.
// If all routers return Skip=true (or none are registered),
// the gateway falls back to static route→pool mapping from config.
type RequestRouter interface {
	Plugin
	Route(req *http.Request, body []byte) (*RoutingDecision, error)
}

// RoutingDecision is the output of a RequestRouter.
type RoutingDecision struct {
	Pool     string // target pool name
	Model    string // model name override (rewrites request body "model" field)
	Endpoint string // specific endpoint (empty = use pool load balancer)
	Reason   string // human-readable explanation for observability
	Skip     bool   // true = this router declines, try next
}

// RequestTransformer modifies the request body before proxying.
// Transformers run in pipeline order; each receives the output of the previous.
// The returned body replaces the request body for downstream processing.
type RequestTransformer interface {
	Plugin
	TransformRequest(req *http.Request, body []byte) ([]byte, error)
}

// ResponseTransformer modifies the upstream response before it reaches the client.
// Runs inside httputil.ReverseProxy.ModifyResponse.
type ResponseTransformer interface {
	Plugin
	TransformResponse(resp *http.Response) error
}

// Authenticator validates incoming requests.
// All registered authenticators must return nil for the request to proceed.
// If any returns an error, the request is rejected with that error's status.
type Authenticator interface {
	Plugin
	Authenticate(req *http.Request) error
}

// AuthError wraps an authentication error with an HTTP status code.
type AuthError struct {
	Code    int
	Message string
}

func (e *AuthError) Error() string { return e.Message }

// AuditSink receives audit events after request completion.
// Sinks are fan-out: every registered sink receives every event.
// Record() must be non-blocking (async/buffered) to avoid stalling the request path.
type AuditSink interface {
	Plugin
	Record(record *AuditRecord) error
}

// MiddlewareProvider supplies HTTP middleware to wrap the handler chain.
// Multiple providers are wrapped in priority order (outermost = lowest priority).
type MiddlewareProvider interface {
	Plugin
	Middleware() func(http.Handler) http.Handler
}

// MetricsHook allows plugins to register custom Prometheus metrics.
// Called once during Init via Context.Metrics.
type MetricsHook interface {
	Plugin
	RegisterMetrics() error
}

// HealthReporter lets plugins contribute to the /health endpoint.
// Status() is called when /health is requested.
type HealthReporter interface {
	Plugin
	Status() interface{}
}
