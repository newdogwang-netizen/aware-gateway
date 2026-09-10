package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
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
	r.Get("/v1/traces", traceQueryHandler(reg))
	r.Get("/v1/traces/{trial}", traceQueryHandler(reg))
	r.Get("/v1/traces/{trial}/summary", traceSummaryHandler(reg))
	r.Get("/v1/episode-events", episodeEventQueryHandler(reg))
	r.Post("/v1/episode-events", episodeEventIngestHandler(reg, logger))
	r.Get("/v1/episode-state", episodeStateQueryHandler(reg))
	r.Get("/v1/episode-sessions", episodeSessionQueryHandler(reg))

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
			Status  string                     `json:"status"`
			Pools   map[string]pool.PoolStatus `json:"pools"`
			Plugins map[string]interface{}     `json:"plugins,omitempty"`
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

// TraceQueryer, TraceFilter, and TraceEntry are defined in the plugin package
// to avoid circular imports. The core server uses them via plugin.Registry.

func traceQueryHandler(reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Find audit sink plugins that implement TraceQueryer
		var queryer plugin.TraceQueryer
		for _, sink := range reg.AuditSinks() {
			if q, ok := sink.(plugin.TraceQueryer); ok {
				queryer = q
				break
			}
		}
		if queryer == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "no audit sink supports trace queries (enable audit plugin with sqlite store)",
			})
			return
		}

		// Parse query params
		filter := plugin.TraceFilter{
			TrialName: r.URL.Query().Get("trial"),
			TaskName:  r.URL.Query().Get("task"),
			StepName:  r.URL.Query().Get("step"),
			SessionID: r.URL.Query().Get("session_id"),
			EpisodeID: r.URL.Query().Get("episode_id"),
		}
		// Also try chi URL param for /v1/traces/{trial}
		if filter.TrialName == "" {
			if rtr := chi.RouteContext(r.Context()); rtr != nil {
				filter.TrialName = rtr.URLParam("trial")
			}
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			var n int
			fmt.Sscanf(l, "%d", &n)
			if n > 0 {
				filter.Limit = n
			}
		}
		if filter.Limit == 0 {
			filter.Limit = 100
		}

		entries, err := queryer.QueryTraces(filter)
		if err != nil {
			slog.Error("trace query failed", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"traces": entries,
			"count":  len(entries),
		})
	}
}

// traceSummaryHandler returns aggregated cost/token stats for a trial.
// GET /v1/traces/{trial}/summary
func traceSummaryHandler(reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var queryer plugin.TraceQueryer
		for _, sink := range reg.AuditSinks() {
			if q, ok := sink.(plugin.TraceQueryer); ok {
				queryer = q
				break
			}
		}
		if queryer == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "no audit sink supports trace queries",
			})
			return
		}

		trialName := ""
		if rtr := chi.RouteContext(r.Context()); rtr != nil {
			trialName = rtr.URLParam("trial")
		}
		if trialName == "" {
			trialName = r.URL.Query().Get("trial")
		}

		// Get all traces for this trial (up to 1000)
		entries, err := queryer.QueryTraces(plugin.TraceFilter{
			TrialName: trialName,
			Limit:     1000,
		})
		if err != nil {
			slog.Error("trace summary query failed", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal"})
			return
		}

		// Aggregate
		totalPrompt := 0
		totalCompletion := 0
		totalTokens := 0
		totalCost := 0.0
		successCount := 0
		failCount := 0
		stepBreakdown := map[string]interface{}{}
		modelBreakdown := map[string]interface{}{}

		for _, e := range entries {
			totalPrompt += e.PromptTokens
			totalCompletion += e.CompTokens
			totalTokens += e.TotalTokens
			totalCost += e.Cost
			if e.Status == 200 {
				successCount++
			} else {
				failCount++
			}

			// Per-step aggregation
			stepKey := e.StepName
			if stepKey == "" {
				stepKey = "(no-step)"
			}
			if existing, ok := stepBreakdown[stepKey].(map[string]interface{}); ok {
				existing["prompt_tokens"] = existing["prompt_tokens"].(int) + e.PromptTokens
				existing["completion_tokens"] = existing["completion_tokens"].(int) + e.CompTokens
				existing["total_tokens"] = existing["total_tokens"].(int) + e.TotalTokens
				existing["cost"] = existing["cost"].(float64) + e.Cost
				existing["calls"] = existing["calls"].(int) + 1
			} else {
				stepBreakdown[stepKey] = map[string]interface{}{
					"prompt_tokens":     e.PromptTokens,
					"completion_tokens": e.CompTokens,
					"total_tokens":      e.TotalTokens,
					"cost":              e.Cost,
					"calls":             1,
					"model":             e.RoutedModel,
					"routing_reason":    e.RoutingReason,
				}
			}

			// Per-model aggregation
			modelKey := e.RoutedModel
			if modelKey == "" {
				modelKey = "(unknown)"
			}
			if existing, ok := modelBreakdown[modelKey].(map[string]interface{}); ok {
				existing["prompt_tokens"] = existing["prompt_tokens"].(int) + e.PromptTokens
				existing["completion_tokens"] = existing["completion_tokens"].(int) + e.CompTokens
				existing["total_tokens"] = existing["total_tokens"].(int) + e.TotalTokens
				existing["cost"] = existing["cost"].(float64) + e.Cost
				existing["calls"] = existing["calls"].(int) + 1
			} else {
				modelBreakdown[modelKey] = map[string]interface{}{
					"prompt_tokens":     e.PromptTokens,
					"completion_tokens": e.CompTokens,
					"total_tokens":      e.TotalTokens,
					"cost":              e.Cost,
					"calls":             1,
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"trial":                   trialName,
			"total_calls":             len(entries),
			"successful_calls":        successCount,
			"failed_calls":            failCount,
			"total_prompt_tokens":     totalPrompt,
			"total_completion_tokens": totalCompletion,
			"total_tokens":            totalTokens,
			"total_cost_usd":          totalCost,
			"per_step":                stepBreakdown,
			"per_model":               modelBreakdown,
		})
	}
}

func episodeEventIngestHandler(reg *plugin.Registry, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !runAuthenticators(reg, w, r) {
			return
		}

		sinks := reg.EpisodeEventSinks()
		if len(sinks) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "no plugin supports episode event ingestion",
			})
			return
		}

		events, err := decodeEpisodeEvents(r)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		for i := range events {
			events[i] = normalizeEpisodeEventFromRequest(events[i], r)
			if err := validateEpisodeEvent(events[i]); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": err.Error(),
					"index": i,
				})
				return
			}
		}

		for i := range events {
			event := &events[i]
			for _, sink := range sinks {
				if err := sink.RecordEpisodeEvent(event); err != nil {
					logger.Warn("episode event sink error",
						"plugin", sink.Name(),
						"event_id", event.EventID,
						"episode_id", event.EpisodeID,
						"error", err,
					)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"error":    "episode event sink failed",
						"event_id": event.EventID,
						"index":    i,
					})
					return
				}
			}
		}

		eventIDs := make([]string, 0, len(events))
		episodeIDs := make([]string, 0, len(events))
		for _, event := range events {
			eventIDs = append(eventIDs, event.EventID)
			episodeIDs = append(episodeIDs, event.EpisodeID)
		}
		response := map[string]interface{}{
			"status":      "accepted",
			"count":       len(events),
			"event_ids":   eventIDs,
			"episode_ids": episodeIDs,
			"sinks":       len(sinks),
		}
		if len(events) == 1 {
			response["event_id"] = events[0].EventID
			response["episode_id"] = events[0].EpisodeID
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(response)
	}
}

func episodeEventQueryHandler(reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var queryer plugin.EpisodeEventQueryer
		for _, p := range reg.AllPlugins() {
			if q, ok := p.(plugin.EpisodeEventQueryer); ok {
				queryer = q
				break
			}
		}
		if queryer == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "no plugin supports episode event queries",
			})
			return
		}

		filter := plugin.EpisodeEventFilter{
			EpisodeID: r.URL.Query().Get("episode_id"),
			SessionID: r.URL.Query().Get("session_id"),
			TrialName: r.URL.Query().Get("trial"),
			Kind:      r.URL.Query().Get("kind"),
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			var n int
			fmt.Sscanf(l, "%d", &n)
			if n > 0 {
				filter.Limit = n
			}
		}
		if filter.Limit == 0 {
			filter.Limit = 100
		}

		events, err := queryer.QueryEpisodeEvents(filter)
		if err != nil {
			slog.Error("episode event query failed", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"events": events,
			"count":  len(events),
		})
	}
}

func episodeStateQueryHandler(reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var queryer plugin.EpisodeStateQueryer
		for _, p := range reg.AllPlugins() {
			if q, ok := p.(plugin.EpisodeStateQueryer); ok {
				queryer = q
				break
			}
		}
		if queryer == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "no plugin supports episode state queries",
			})
			return
		}

		filter := plugin.EpisodeStateFilter{
			EpisodeID: r.URL.Query().Get("episode_id"),
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			var n int
			fmt.Sscanf(l, "%d", &n)
			if n > 0 {
				filter.Limit = n
			}
		}
		if filter.Limit == 0 {
			filter.Limit = 100
		}

		states, err := queryer.QueryEpisodeStates(filter)
		if err != nil {
			slog.Error("episode state query failed", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"states": states,
			"count":  len(states),
		})
	}
}

func episodeSessionQueryHandler(reg *plugin.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var queryer plugin.EpisodeSessionQueryer
		for _, p := range reg.AllPlugins() {
			if q, ok := p.(plugin.EpisodeSessionQueryer); ok {
				queryer = q
				break
			}
		}
		if queryer == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "no plugin supports episode session queries",
			})
			return
		}

		filter := plugin.EpisodeSessionFilter{
			SessionID: r.URL.Query().Get("session_id"),
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			var n int
			fmt.Sscanf(l, "%d", &n)
			if n > 0 {
				filter.Limit = n
			}
		}
		if filter.Limit == 0 {
			filter.Limit = 100
		}

		sessions, err := queryer.QueryEpisodeSessions(filter)
		if err != nil {
			slog.Error("episode session query failed", "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sessions": sessions,
			"count":    len(sessions),
		})
	}
}

func runAuthenticators(reg *plugin.Registry, w http.ResponseWriter, r *http.Request) bool {
	for _, auth := range reg.Authenticators() {
		if err := auth.Authenticate(r); err != nil {
			code := http.StatusUnauthorized
			if ae, ok := err.(*plugin.AuthError); ok {
				code = ae.Code
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return false
		}
	}
	return true
}

func decodeEpisodeEvents(r *http.Request) ([]plugin.EpisodeEvent, error) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid episode event json")
	}
	first := byte(0)
	for _, b := range raw {
		switch b {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			first = b
		}
		break
	}
	if first == 0 {
		return nil, fmt.Errorf("episode event body is required")
	}
	if first == '[' {
		var events []plugin.EpisodeEvent
		if err := json.Unmarshal(raw, &events); err != nil {
			return nil, fmt.Errorf("invalid episode event json")
		}
		if len(events) == 0 {
			return nil, fmt.Errorf("events must not be empty")
		}
		return events, nil
	}
	if first != '{' {
		return nil, fmt.Errorf("invalid episode event json")
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("invalid episode event json")
	}
	if _, ok := object["events"]; ok {
		var envelope struct {
			Events []plugin.EpisodeEvent `json:"events"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, fmt.Errorf("invalid episode event json")
		}
		if len(envelope.Events) == 0 {
			return nil, fmt.Errorf("events must not be empty")
		}
		return envelope.Events, nil
	}

	var event plugin.EpisodeEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("invalid episode event json")
	}
	return []plugin.EpisodeEvent{event}, nil
}

func normalizeEpisodeEventFromRequest(event plugin.EpisodeEvent, r *http.Request) plugin.EpisodeEvent {
	if event.SchemaVersion == "" {
		event.SchemaVersion = "event-schema-v1"
	}
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
		event.TimestampSource = "gateway_received_at"
	}
	if event.EpisodeID == "" {
		event.EpisodeID = r.Header.Get("X-Episode-ID")
	}
	if event.EpisodeOp == "" {
		event.EpisodeOp = r.Header.Get("X-Episode-Operation")
	}
	if event.SessionID == "" {
		event.SessionID = r.Header.Get("X-Session-ID")
	}
	if event.TrialName == "" {
		event.TrialName = r.Header.Get("X-Trial-Name")
	}
	if event.StepName == "" {
		event.StepName = r.Header.Get("X-Step-Name")
	}
	if event.TaskName == "" {
		event.TaskName = r.Header.Get("X-Task-Name")
	}
	if event.EpisodeID == "" {
		event.EpisodeID = event.SessionID
	}
	if event.EpisodeID == "" {
		event.EpisodeID = event.TrialName
	}
	if event.Source == "" {
		event.Source = "gateway_episode_events_api"
	}
	if event.Certainty == "" {
		event.Certainty = "observed"
	}
	if event.ExtractorVersion == "" {
		event.ExtractorVersion = "online-gateway-v1"
	}
	if event.Observation == nil {
		event.Observation = map[string]any{}
	}
	if event.EvidenceRefs == nil {
		event.EvidenceRefs = []string{}
	}
	return event
}

func validateEpisodeEvent(event plugin.EpisodeEvent) error {
	if event.EpisodeID == "" {
		return fmt.Errorf("episode_id is required")
	}
	if event.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	switch event.Kind {
	case "llm_call",
		"tool_call",
		"file_written",
		"file_modified",
		"test_run",
		"test_failed",
		"test_passed",
		"verifier_result",
		"no_progress",
		"run_exception":
	default:
		return fmt.Errorf("unsupported episode event kind %q", event.Kind)
	}
	if event.Source == "" {
		return fmt.Errorf("source is required")
	}
	if event.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	return nil
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
