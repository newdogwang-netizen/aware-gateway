// Package core contains the gateway's request processing engine.
//
// The handler integrates the plugin system with the proxy engine:
//  1. Authenticator plugins validate the request
//  2. RequestRouter plugins decide which pool/model to use (chain-of-responsibility)
//  3. RequestTransformer plugins modify the request body
//  4. The pool proxy forwards with retry + circuit breaker + fallback
//  5. ResponseTransformer plugins modify the response
//  6. AuditSink plugins receive the completed audit record
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/metrics"
	"github.com/aware/gateway/internal/plugin"
	"github.com/aware/gateway/internal/pool"
	"github.com/aware/gateway/internal/proxy"
	"github.com/aware/gateway/internal/routing"
)

const maxBodySize = 50 << 20 // 50MB
const maxRetryBufSize = 1 << 20

// TaskContext holds task/step correlation metadata extracted from request headers.
// Harbor agents pass these via LiteLLM extra_headers — they allow the gateway
// to group multiple LLM calls into a single task run / step and answer
// "which model did step N use?"
type TaskContext struct {
	SessionID string // X-Session-ID (e.g. "{trial_name}__agent")
	TrialName string // X-Trial-Name (e.g. "trial-abc123")
	StepName  string // X-Step-Name (e.g. "fix-bug")
	TaskName  string // X-Task-Name (e.g. "data-anonymization")
}

// PoolProvider abstracts pool lookup for hot-reload support.
type PoolProvider interface {
	Get(name string) (*pool.Pool, bool)
	All() map[string]*pool.Pool
}

// MapPoolProvider wraps a plain map for tests.
type MapPoolProvider map[string]*pool.Pool

func (m MapPoolProvider) Get(name string) (*pool.Pool, bool) {
	p, ok := m[name]
	return p, ok
}

func (m MapPoolProvider) All() map[string]*pool.Pool {
	out := make(map[string]*pool.Pool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Handler is the core request processor. It is created once and used
// as the HTTP handler for all proxied routes.
type Handler struct {
	cfg       *config.Config
	pp        PoolProvider
	registry  *plugin.Registry
	logger    *slog.Logger
	retryable map[int]bool
	maxRetries int
}

// NewHandler creates the core handler.
func NewHandler(cfg *config.Config, pp PoolProvider, reg *plugin.Registry, logger *slog.Logger) *Handler {
	retryable := make(map[int]bool, len(cfg.Retry.RetryableStatuses))
	for _, s := range cfg.Retry.RetryableStatuses {
		retryable[s] = true
	}
	maxRetries := cfg.Retry.MaxRetries
	if maxRetries < 1 {
		maxRetries = 1
	}
	return &Handler{
		cfg:        cfg,
		pp:         pp,
		registry:   reg,
		logger:     logger,
		retryable:  retryable,
		maxRetries: maxRetries,
	}
}

// ServeHTTP is the main entry point for proxied routes.
// It runs the full plugin chain: auth → route → transform → proxy → audit.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// --- 1. Trace ID ---
	traceID := r.Header.Get("X-Request-Id")
	if traceID == "" {
		traceID = r.Header.Get("X-Trace-ID")
	}
	if traceID == "" {
		traceID = uuid.NewString()
	}

	// Extract task/step correlation headers (passed by harbor agents via LiteLLM extra_headers)
	taskCtx := TaskContext{
		SessionID: r.Header.Get("X-Session-ID"),
		TrialName: r.Header.Get("X-Trial-Name"),
		StepName:  r.Header.Get("X-Step-Name"),
		TaskName:  r.Header.Get("X-Task-Name"),
	}

	// --- 2. Authenticators ---
	for _, auth := range h.registry.Authenticators() {
		if err := auth.Authenticate(r); err != nil {
			code := http.StatusUnauthorized
			if ae, ok := err.(*plugin.AuthError); ok {
				code = ae.Code
			}
			slog.Warn("auth rejected",
				"plugin", auth.Name(),
				"trace_id", traceID,
				"status", code,
				"error", err,
			)
			http.Error(w, err.Error(), code)
			return
		}
	}

	// --- 3. Buffer request body ---
	var bodyBytes []byte
	if cached := routing.GetBodyBytes(r); cached != nil {
		bodyBytes = cached
	} else if r.Body != nil {
		limited := io.LimitReader(r.Body, maxBodySize+1)
		var err error
		bodyBytes, err = io.ReadAll(limited)
		r.Body.Close()
		if err != nil {
			slog.Error("failed to read request body", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		r = routing.SetBodyBytes(r, bodyBytes)
	}

	// Extract original model from body (for audit)
	originalModel := extractModel(bodyBytes, r.Header.Get("Content-Type"))

	// --- 4. Request Routers (chain-of-responsibility) ---
	routedPool := ""
	routedModel := ""
	routedEndpoint := ""
	routingReason := ""

	routers := h.registry.Routers()
	if len(routers) > 0 && len(bodyBytes) > 0 && len(bodyBytes) <= maxBodySize {
		for _, router := range routers {
			decision, err := router.Route(r, bodyBytes)
			if err != nil {
				slog.Warn("router error",
					"plugin", router.Name(),
					"error", err,
				)
				continue
			}
			if decision != nil && !decision.Skip {
				routedPool = decision.Pool
				routedModel = decision.Model
				routedEndpoint = decision.Endpoint
				routingReason = decision.Reason
				slog.Info("routing decision",
					"router", router.Name(),
					"pool", routedPool,
					"model", routedModel,
					"endpoint", routedEndpoint,
					"reason", routingReason,
				)
				metrics.RoutingDecisionTotal.WithLabelValues(
					router.Name(), routedPool, routedModel, routingReason,
				).Inc()
				break
			}
		}
	}

	// --- 5. Apply request transformers ---
	transformers := h.registry.RequestTransformers()
	for _, t := range transformers {
		if len(bodyBytes) > 0 {
			transformed, err := t.TransformRequest(r, bodyBytes)
			if err != nil {
				slog.Warn("request transform error",
					"plugin", t.Name(),
					"error", err,
				)
				continue
			}
			bodyBytes = transformed
			r = routing.SetBodyBytes(r, bodyBytes)
		}
	}

	// --- 6. Apply model override (from router) ---
	finalModel := originalModel
	if routedModel != "" {
		bodyBytes = rewriteModel(bodyBytes, routedModel)
		r = routing.SetBodyBytes(r, bodyBytes)
		finalModel = routedModel
	} else if len(h.cfg.ModelMap) > 0 && originalModel != "" {
		if mapped, ok := h.cfg.ModelMap[originalModel]; ok {
			bodyBytes = rewriteModel(bodyBytes, mapped)
			r = routing.SetBodyBytes(r, bodyBytes)
			finalModel = mapped
		}
	}

	// --- 7. Determine target pool ---
	targetPool := routedPool
	if targetPool == "" {
		// Fall back to static route mapping
		routePattern := chi.RouteContext(r.Context()).RoutePattern()
		targetPool = h.poolForRoute(routePattern, r.URL.Path)
	}
	if targetPool == "" {
		http.Error(w, "no pool for route", http.StatusBadGateway)
		return
	}

	primaryPool, ok := h.pp.Get(targetPool)
	if !ok {
		http.Error(w, "pool not found", http.StatusBadGateway)
		return
	}

	// --- 8. Proxy with retry + breaker + fallback ---
	// Install mutable meta holder for audit
	r, meta := routing.WithMeta(r)
	routing.SetPool(r, targetPool)
	if finalModel != "" {
		routing.SetRoutedModel(r, finalModel, routingReason)
	}

	dw := &decisionWriter{
		real:      w,
		retryable: h.retryable,
		header:    make(http.Header),
	}

	h.proxyWithRetry(dw, r, primaryPool, h.pp, bodyBytes, routedEndpoint, finalModel, meta, targetPool)

	// --- 9. Audit ---
	h.recordAudit(
		traceID, start, r, dw, meta, targetPool, originalModel, finalModel, routingReason, taskCtx,
	)
}

// proxyWithRetry handles the retry + circuit breaker + fallback logic.
// If routedEndpoint is non-empty, it tries that specific endpoint first.
// If routedModel is non-empty, endpoint selection is model-aware (only picks
// endpoints that serve that model).
func (h *Handler) proxyWithRetry(
	dw *decisionWriter,
	r *http.Request,
	primaryPool *pool.Pool,
	pp PoolProvider,
	bodyBytes []byte,
	routedEndpoint string,
	routedModel string,
	meta *routing.Meta,
	poolName string,
) {
	fallbackPoolName := primaryPool.Fallback
	var fallbackPool *pool.Pool
	if fallbackPoolName != "" {
		fallbackPool, _ = pp.Get(fallbackPoolName)
	}

	poolsToTry := []*pool.Pool{primaryPool}
	if fallbackPool != nil {
		poolsToTry = append(poolsToTry, fallbackPool)
	}

	for _, currentPool := range poolsToTry {
		for attempt := 0; attempt < h.maxRetries; attempt++ {
			var ep *pool.Endpoint
			if attempt == 0 && routedEndpoint != "" {
				ep = currentPool.SelectEndpoint(routedEndpoint)
			}
			if ep == nil {
				// Model-aware endpoint selection: if we have a routed model,
				// only pick endpoints that serve it
				if routedModel != "" {
					ep = currentPool.NextForModel(routedModel)
				} else {
					ep = currentPool.Next()
				}
			}
			if ep == nil {
				slog.Warn("pool has no available endpoints",
					"pool", currentPool.Name,
					"attempt", attempt+1,
				)
				break
			}
			ep.Inc()
			metrics.ActiveRequests.WithLabelValues(currentPool.Name).Inc()

			if bodyBytes != nil {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			} else {
				r.Body = http.NoBody
			}

			routing.SetMeta(r, ep.Name, attempt+1, currentPool != primaryPool)

			span := trace.SpanFromContext(r.Context())
			span.SetAttributes(
				attribute.String("pool.name", currentPool.Name),
				attribute.String("pool.endpoint", ep.Name),
				attribute.Int("pool.attempt", attempt+1),
				attribute.Bool("pool.fallback", currentPool != primaryPool),
			)

			// Reset decisionWriter for this attempt
			dw.reset()

			breakerErr := error(nil)
			_, err := ep.ExecuteWithBreaker(func() (struct{}, error) {
				p := proxy.NewReverseProxy(ep)
				p.ServeHTTP(dw, r)
				if dw.code >= 500 || dw.code == http.StatusTooManyRequests {
					return struct{}{}, fmt.Errorf("upstream returned %d", dw.code)
				}
				if dw.code == 0 {
					return struct{}{}, fmt.Errorf("upstream proxy error")
				}
				return struct{}{}, nil
			})
			breakerErr = err

			ep.Dec()
			metrics.ActiveRequests.WithLabelValues(currentPool.Name).Dec()

			if breakerErr != nil {
				if errors.Is(breakerErr, gobreaker.ErrOpenState) {
					slog.Warn("circuit breaker open, skipping endpoint",
						"endpoint", ep.Name,
						"attempt", attempt+1,
					)
					continue
				}
				slog.Warn("breaker rejected request",
					"endpoint", ep.Name,
					"error", breakerErr,
				)
				continue
			}

			if dw.IsBuffered() && h.retryable[dw.code] && attempt < h.maxRetries-1 {
				slog.Info("retrying on upstream error",
					"endpoint", ep.Name,
					"status", dw.code,
					"attempt", attempt+1,
				)
				continue
			}

			dw.commit()
			return
		}

		if currentPool == primaryPool && fallbackPool != nil {
			slog.Warn("primary pool exhausted, trying fallback",
				"primary", primaryPool.Name,
				"fallback", fallbackPool.Name,
			)
			continue
		}
	}

	slog.Error("all pools and retries exhausted",
		"primary", primaryPool.Name,
		"max_retries", h.maxRetries,
	)
	dw.code = http.StatusServiceUnavailable
	dw.commit()
}

// poolForRoute finds the pool for a given route pattern or path.
func (h *Handler) poolForRoute(pattern, path string) string {
	// Try exact pattern match first
	if pattern != "" {
		for _, r := range h.cfg.Routes {
			if r.Pattern == pattern {
				return r.Pool
			}
		}
	}
	// Fall back to path prefix matching
	for _, r := range h.cfg.Routes {
		if strings.HasPrefix(path, r.Pattern) {
			return r.Pool
		}
	}
	return ""
}

// recordAudit dispatches the audit record to all registered sinks.
func (h *Handler) recordAudit(
	traceID string,
	start time.Time,
	r *http.Request,
	dw *decisionWriter,
	meta *routing.Meta,
	poolName string,
	originalModel, finalModel, routingReason string,
	taskCtx TaskContext,
) {
	endpoint, retryAttempt, isFallback := routing.GetRoutingMeta(r)
	if meta != nil && meta.Endpoint != "" {
		endpoint = meta.Endpoint
		retryAttempt = meta.Attempt
		if meta.Fallback {
			isFallback = "true"
		} else {
			isFallback = ""
		}
	}

	promptTokens, compTokens, totalTokens := dw.promptTokens, dw.compTokens, dw.totalTokens
	hasTokens := dw.hasTokens

	if !hasTokens && dw.streaming {
		if p, c, t, ok := parseSSEUsage(dw.tail); ok {
			promptTokens, compTokens, totalTokens = p, c, t
			hasTokens = true
		}
	}

	finishReason := dw.finishReason
	if finishReason == "" && dw.streaming {
		finishReason = parseSSEFinishReason(dw.tail)
	}

	// Record Prometheus metrics
	routePattern := chi.RouteContext(r.Context()).RoutePattern()
	if routePattern == "" {
		routePattern = r.URL.Path
	}
	statusStr := strconv.Itoa(dw.code)
	metrics.RequestTotal.WithLabelValues(poolName, routePattern, endpoint, finalModel, statusStr).Inc()
	metrics.RequestDuration.WithLabelValues(poolName, routePattern, endpoint).Observe(
		time.Since(start).Seconds(),
	)
	if retryAttempt > 1 {
		metrics.RetryTotal.WithLabelValues(poolName, endpoint).Inc()
	}
	if isFallback != "" {
		metrics.FallbackTotal.WithLabelValues(poolName).Inc()
	}
	if hasTokens {
		metrics.TokensTotal.WithLabelValues(poolName, endpoint, finalModel, "prompt").Add(float64(promptTokens))
		metrics.TokensTotal.WithLabelValues(poolName, endpoint, finalModel, "completion").Add(float64(compTokens))
		metrics.TokensTotal.WithLabelValues(poolName, endpoint, finalModel, "total").Add(float64(totalTokens))
	}
	if dw.streaming && !dw.firstWriteAt.IsZero() && dw.code < 400 {
		metrics.LLMTTFT.WithLabelValues(poolName, finalModel).Observe(dw.firstWriteAt.Sub(start).Seconds())
	}

	// Build audit record
	record := &plugin.AuditRecord{
		TraceID:       traceID,
		Timestamp:     start,
		Method:        r.Method,
		Path:          r.URL.Path,
		Endpoint:      endpoint,
		Status:        dw.code,
		LatencyMs:     time.Since(start).Milliseconds(),
		Model:         originalModel,
		RoutedModel:   finalModel,
		Pool:          poolName,
		PromptTokens:  promptTokens,
		CompTokens:    compTokens,
		TotalTokens:   totalTokens,
		RetryAttempt:  retryAttempt,
		Fallback:      isFallback,
		Streaming:     dw.streaming,
		FinishReason:  finishReason,
		RoutingReason: routingReason,
		ErrorKind:     classifyError(dw.code),
		SessionID:     taskCtx.SessionID,
		TrialName:     taskCtx.TrialName,
		StepName:      taskCtx.StepName,
		TaskName:      taskCtx.TaskName,
	}

	// Dispatch to audit sinks
	for _, sink := range h.registry.AuditSinks() {
		if err := sink.Record(record); err != nil {
			slog.Warn("audit sink error",
				"plugin", sink.Name(),
				"error", err,
			)
		}
	}
}

// --- Helpers ---

func extractModel(body []byte, contentType string) string {
	if !strings.HasPrefix(contentType, "application/json") || len(body) == 0 {
		return ""
	}
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) == nil {
		return req.Model
	}
	return ""
}

func rewriteModel(body []byte, newModel string) []byte {
	if len(body) == 0 || newModel == "" {
		return body
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	req["model"] = newModel
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

func classifyError(status int) string {
	switch {
	case status == 0:
		return "proxy_error"
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "upstream_error"
	case status >= 400:
		return "client_error"
	default:
		return ""
	}
}

// --- SSE parsing ---

const maxTailBytes = 64 << 10

func parseSSEUsage(tail []byte) (prompt, completion, total int, ok bool) {
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || data[0] != '{' {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &chunk) != nil || chunk.Usage == nil {
			continue
		}
		if chunk.Usage.TotalTokens > 0 {
			prompt, completion, total, ok = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens, true
		}
	}
	return
}

func parseSSEFinishReason(tail []byte) string {
	var reason string
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || data[0] != '{' {
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		if r := chunk.Choices[0].FinishReason; r != "" {
			reason = r
		}
	}
	return reason
}
