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
//   - RequestRouter: chain-of-responsibility — first non-Skip decision wins
//   - RequestTransformer: pipeline — each transformer runs in order
//   - ResponseTransformer: pipeline — each transformer runs in order
//   - Authenticator: all must pass (AND logic)
//   - AuditSink: fan-out — each sink receives every event
//   - EpisodeEventSink: fan-out — each sink receives explicit progress events
//   - MiddlewareProvider: middleware wrapped in registration order
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
	Pool                string // target pool name
	Model               string // model name override (rewrites request body "model" field)
	Endpoint            string // specific endpoint (empty = use pool load balancer)
	Reason              string // human-readable explanation for observability
	BudgetAction        string // optional route action profile, e.g. cheap_probe
	MaxTokens           int    // optional request max_tokens override
	TimeoutMs           int    // optional per-attempt upstream timeout override
	EpisodeID           string // task episode used for stateful routing
	EpisodeOperation    string // continue / interrupt / resume / global / unknown
	EpisodeStateVersion int    // episode state version observed before routing
	EpisodeStateBefore  string // compact JSON state observed before routing
	Skip                bool   // true = this router declines, try next
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

// EpisodeEventSink receives explicit episode outcome/progress events. These
// events cover non-LLM work such as tool calls, file writes, test runs, and
// verifier results.
type EpisodeEventSink interface {
	Plugin
	RecordEpisodeEvent(event *EpisodeEvent) error
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

// TraceFilter holds query parameters for trace lookup.
type TraceFilter struct {
	TrialName string
	TaskName  string
	StepName  string
	SessionID string
	EpisodeID string
	Limit     int
}

// TraceEntry is a single LLM call record returned by /v1/traces.
type TraceEntry struct {
	TraceID        string  `json:"trace_id"`
	Timestamp      string  `json:"timestamp"`
	Model          string  `json:"model"`
	RoutedModel    string  `json:"routed_model"`
	Pool           string  `json:"pool"`
	Endpoint       string  `json:"endpoint"`
	StepName       string  `json:"step_name,omitempty"`
	TaskName       string  `json:"task_name,omitempty"`
	TrialName      string  `json:"trial_name,omitempty"`
	SessionID      string  `json:"session_id,omitempty"`
	PromptTokens   int     `json:"prompt_tokens"`
	CompTokens     int     `json:"completion_tokens"`
	TotalTokens    int     `json:"total_tokens"`
	Cost           float64 `json:"cost"`
	LatencyMs      int64   `json:"latency_ms"`
	Status         int     `json:"status"`
	Streaming      bool    `json:"streaming"`
	FinishReason   string  `json:"finish_reason,omitempty"`
	RoutingReason  string  `json:"routing_reason,omitempty"`
	BudgetAction   string  `json:"route_budget_action,omitempty"`
	RouteMaxTokens int     `json:"route_max_tokens,omitempty"`
	RouteTimeoutMs int     `json:"route_timeout_ms,omitempty"`
	EpisodeID      string  `json:"episode_id,omitempty"`
	EpisodeOp      string  `json:"episode_operation,omitempty"`
	StateVersion   int     `json:"episode_state_version,omitempty"`
	StateBefore    string  `json:"episode_state_before,omitempty"`
	StateAfter     string  `json:"episode_state_after,omitempty"`
}

// EpisodeEventFilter holds query parameters for episode event lookup.
type EpisodeEventFilter struct {
	EpisodeID string
	Kind      string
	Limit     int
}

// EpisodeStateFilter holds query parameters for online episode state lookup.
type EpisodeStateFilter struct {
	EpisodeID string
	Limit     int
}

// EpisodeSessionFilter holds query parameters for current task-line stacks.
type EpisodeSessionFilter struct {
	SessionID string
	Limit     int
}

// EpisodeStateEntry is a compact, queryable state projection for one task
// episode. State is intentionally extensible because reducer dimensions evolve
// as policy experiments add new signals.
type EpisodeStateEntry struct {
	EpisodeID    string         `json:"episode_id"`
	StateVersion int            `json:"state_version"`
	Source       string         `json:"source"`
	State        map[string]any `json:"state"`
}

// EpisodeSessionEntry exposes the current episode stack for one user/session.
// It lets operators verify which task line the stateful router is using before
// inspecting the per-episode reducer state.
type EpisodeSessionEntry struct {
	SessionID       string         `json:"session_id"`
	ActiveEpisodeID string         `json:"active_episode_id"`
	EpisodeStack    []string       `json:"episode_stack"`
	StackDepth      int            `json:"stack_depth"`
	NextEpisode     int            `json:"next_episode_index"`
	Version         int            `json:"version"`
	LastOperation   string         `json:"last_operation"`
	LastConfidence  float64        `json:"last_confidence"`
	LastEvidence    []string       `json:"last_evidence"`
	UpdatedAt       string         `json:"updated_at,omitempty"`
	Source          string         `json:"source"`
	State           map[string]any `json:"state,omitempty"`
}

// TraceQueryer is an optional interface that AuditSink plugins can implement
// to support the /v1/traces endpoint.
type TraceQueryer interface {
	QueryTraces(filter TraceFilter) ([]TraceEntry, error)
}

// EpisodeEventQueryer is an optional interface for plugins that can retrieve
// explicit outcome/progress events.
type EpisodeEventQueryer interface {
	QueryEpisodeEvents(filter EpisodeEventFilter) ([]EpisodeEvent, error)
}

// EpisodeStateQueryer is an optional interface for plugins that can expose the
// current reduced task episode state.
type EpisodeStateQueryer interface {
	QueryEpisodeStates(filter EpisodeStateFilter) ([]EpisodeStateEntry, error)
}

// EpisodeSessionQueryer is an optional interface for plugins that can expose
// current session-to-episode stack state.
type EpisodeSessionQueryer interface {
	QueryEpisodeSessions(filter EpisodeSessionFilter) ([]EpisodeSessionEntry, error)
}
