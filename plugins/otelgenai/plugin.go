// Package otelgenai implements a GenAI observability plugin for the aware-gateway.
//
// It enriches OpenTelemetry trace spans with GenAI semantic conventions
// (gen_ai.* and/or llm.*) and records GenAI-specific Prometheus metrics,
// following the same approach as Envoy AI Gateway.
//
// The plugin implements:
//   - MiddlewareProvider: wraps the handler to create GenAI-aware spans
//   - AuditSink: receives completed records and emits GenAI metrics
//   - HealthReporter: reports observability status
//
// Configuration (under plugins.otel-genai in gateway.yaml):
//
//	plugins:
//	  otel-genai:
//	    enabled: true
//	    convention: "gen_ai"     # gen_ai | openinference | both
//	    capture_request_body: false   # emit message content in spans (PII risk!)
//	    system_overrides:             # override auto-detected system per pool
//	      vllm: "vllm"
//	      openai-pool: "openai"
package otelgenai

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/metrics"
	"github.com/aware/gateway/internal/otel/genai"
	"github.com/aware/gateway/internal/plugin"
	"github.com/aware/gateway/internal/routing"
)

// Config is the plugin-specific configuration.
type Config struct {
	Enabled            bool              `yaml:"enabled"`
	Convention         string            `yaml:"convention"` // gen_ai | openinference | both
	CaptureRequestBody bool              `yaml:"capture_request_body"`
	SystemOverrides    map[string]string `yaml:"system_overrides"`
}

// Plugin implements GenAI observability via OTel semantic conventions.
type Plugin struct {
	cfg     Config
	conv    genai.Convention
	logger  *slog.Logger
	ctx     *plugin.Context
}

func (p *Plugin) Name() string { return "otel-genai" }

func (p *Plugin) Init(ctx *plugin.Context) error {
	p.ctx = ctx
	p.logger = ctx.Logger

	cfg, ok := config.PluginConfig[Config](ctx.Config, "otel-genai")
	if !ok {
		// Default: enabled with gen_ai convention
		p.cfg = Config{Enabled: true, Convention: "gen_ai"}
	} else {
		p.cfg = cfg
	}

	if !p.cfg.Enabled {
		p.logger.Info("otel-genai: disabled in config")
		return nil
	}

	// Parse convention
	switch p.cfg.Convention {
	case "openinference", "llm":
		p.conv = genai.ConvOpenInference
	case "both":
		p.conv = genai.ConvBoth
	default:
		p.conv = genai.ConvOTelGenAI
	}

	p.logger.Info("otel-genai initialized",
		"convention", p.conv,
		"capture_request_body", p.cfg.CaptureRequestBody,
		"system_overrides", len(p.cfg.SystemOverrides),
	)
	return nil
}

func (p *Plugin) Close() error { return nil }

// Middleware implements plugin.MiddlewareProvider.
// The middleware:
//  1. Reads and restores the request body to extract GenAI request attributes
//  2. Enriches the active span with gen_ai.* / llm.* attributes
//  3. Wraps the ResponseWriter to capture response metadata (X-Gw-* headers)
//  4. After handler completion, sets response-side span attributes and records metrics
func (p *Plugin) Middleware() func(http.Handler) http.Handler {
	if !p.cfg.Enabled {
		return nil
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only instrument JSON LLM requests (skip health, metrics, etc.)
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") && r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()

			// --- Request side: extract GenAI attributes ---
			var reqAttrs *genai.RequestAttrs
			var bodyBytes []byte

			if strings.HasPrefix(ct, "application/json") && r.Body != nil {
				// Read body (bounded to avoid OOM on huge requests)
				limited := io.LimitReader(r.Body, 1<<20) // 1MB max for attribute parsing
				bodyBytes, _ = io.ReadAll(limited)
				r.Body.Close()

				// Restore full body for downstream handlers
				// If we read less than the limit, we got the whole body
				if len(bodyBytes) < 1<<20 {
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				} else {
					// Body was truncated — concatenate with remaining
					r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(bodyBytes), r.Body))
				}

				reqAttrs = genai.ParseRequestAttrs(bodyBytes)
			}

			// Override system from config if mapping exists
			if reqAttrs != nil && p.cfg.SystemOverrides != nil {
				// Try to match by pool name — but we don't know the pool yet
				// (router hasn't run). We'll apply override in the response phase.
			}

			// Enrich active span with request attributes
			span := trace.SpanFromContext(r.Context())
			if reqAttrs != nil {
				span.SetAttributes(genai.RequestSpanAttrs(reqAttrs, p.conv)...)
			}

			// --- Wrap ResponseWriter to capture response metadata ---
			gw := &genaiResponseWriter{
				ResponseWriter: w,
				start:          start,
			}

			// --- Call next handler ---
			next.ServeHTTP(gw, r)

			// --- Response side: enrich span with response attributes ---
			responseSystem := ""
			if reqAttrs != nil {
				responseSystem = reqAttrs.System
			}

			// Apply system override based on the pool that actually served
			meta := routing.MetaFrom(r)
			if meta != nil && p.cfg.SystemOverrides != nil {
				if override, ok := p.cfg.SystemOverrides[meta.Pool]; ok {
					responseSystem = override
				}
			}

			// Build response attrs from captured headers
			respAttrs := &genai.ResponseAttrs{
				InputTokens:  gw.promptTokens,
				OutputTokens: gw.compTokens,
			}
			if gw.finishReason != "" {
				respAttrs.FinishReasons = []string{gw.finishReason}
			}

			// For non-streaming, try to parse response body for model/id/fingerprint
			if !gw.streaming && len(gw.body) > 0 {
				if parsed := genai.ParseResponseAttrs(gw.body); parsed != nil {
					if parsed.Model != "" {
						respAttrs.Model = parsed.Model
					}
					if parsed.ResponseID != "" {
						respAttrs.ResponseID = parsed.ResponseID
					}
					if len(parsed.FinishReasons) > 0 {
						respAttrs.FinishReasons = parsed.FinishReasons
					}
					if parsed.SystemFingerprint != "" {
						respAttrs.SystemFingerprint = parsed.SystemFingerprint
					}
					if parsed.InputTokens > 0 {
						respAttrs.InputTokens = parsed.InputTokens
					}
					if parsed.OutputTokens > 0 {
						respAttrs.OutputTokens = parsed.OutputTokens
					}
				}
			}

			// Set response span attributes
			if responseSystem != "" {
				span.SetAttributes(attribute.String(genai.AttrGenAISystem, responseSystem))
			}
			span.SetAttributes(genai.ResponseSpanAttrs(respAttrs, p.conv)...)

			// Set gateway-specific attributes from routing meta
			if meta != nil {
				span.SetAttributes(genai.GatewaySpanAttrs(
					meta.Pool, meta.Endpoint, meta.RoutedModel, meta.RoutingReason,
					meta.Attempt, meta.Fallback,
				)...)
			}

			// TTFT
			if gw.streaming && !gw.firstWriteAt.IsZero() && gw.status < 400 {
				ttft := gw.firstWriteAt.Sub(start).Seconds()
				span.SetAttributes(attribute.Float64(genai.AttrGwTTFT, ttft))
			}

			// Set span status based on response status
			if gw.status >= 500 {
				span.SetAttributes(attribute.String("gen_ai.error.type", classifyError(gw.status)))
			}

			// --- Record GenAI metrics ---
			p.recordMetrics(reqAttrs, respAttrs, responseSystem, gw, start, meta)
		})
	}
}

// recordMetrics emits GenAI-specific Prometheus metrics.
func (p *Plugin) recordMetrics(
	reqAttrs *genai.RequestAttrs,
	respAttrs *genai.ResponseAttrs,
	system string,
	gw *genaiResponseWriter,
	start time.Time,
	meta *routing.Meta,
) {
	if reqAttrs == nil {
		return
	}

	if system == "" {
		system = reqAttrs.System
	}
	operation := reqAttrs.Operation
	if operation == "" {
		operation = "chat"
	}
	model := reqAttrs.Model
	if meta != nil && meta.RoutedModel != "" {
		model = meta.RoutedModel
	}
	if respAttrs.Model != "" {
		model = respAttrs.Model
	}
	statusStr := strconv.Itoa(gw.status)

	// gen_ai_request_total
	metrics.GenAIRequestTotal.WithLabelValues(system, operation, model, statusStr).Inc()

	// gen_ai_tokens_total
	if respAttrs.InputTokens > 0 {
		metrics.GenAITokensTotal.WithLabelValues(system, model, "input").Add(float64(respAttrs.InputTokens))
	}
	if respAttrs.OutputTokens > 0 {
		metrics.GenAITokensTotal.WithLabelValues(system, model, "output").Add(float64(respAttrs.OutputTokens))
	}

	// gen_ai_ttft_seconds (streaming only)
	if gw.streaming && !gw.firstWriteAt.IsZero() && gw.status < 400 {
		metrics.GenAITTFTSeconds.WithLabelValues(system, model).Observe(gw.firstWriteAt.Sub(start).Seconds())
	}

	// gen_ai_request_duration_seconds
	metrics.GenAIRequestDuration.WithLabelValues(system, operation, model).Observe(
		time.Since(start).Seconds(),
	)

	// gen_ai_cost_total (if we have token counts and pricing info)
	// Cost calculation is done in the billing plugin via AuditSink;
	// here we just record what we can compute from response tokens.
	// The billing plugin will add the precise cost to the audit record.
}

// --- HealthReporter ---

func (p *Plugin) Status() interface{} {
	return map[string]interface{}{
		"enabled":    p.cfg.Enabled,
		"convention": string(p.conv),
	}
}

// --- genaiResponseWriter ---

type genaiResponseWriter struct {
	http.ResponseWriter
	status        int
	promptTokens  int
	compTokens    int
	totalTokens   int
	finishReason  string
	streaming     bool
	body          []byte // captured for non-streaming responses
	firstWriteAt  time.Time
	start         time.Time
	wroteHeader   bool
}

const gwMaxBodyCapture = 256 << 10 // 256KB max response body capture for attribute parsing

func (w *genaiResponseWriter) WriteHeader(code int) {
	if code == http.StatusContinue {
		return
	}
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.streaming = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
	w.interceptHeaders()
	w.ResponseWriter.WriteHeader(code)
}

func (w *genaiResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.streaming = strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
		w.interceptHeaders()
		w.status = 200
		w.wroteHeader = true
	}
	if w.firstWriteAt.IsZero() && len(b) > 0 {
		w.firstWriteAt = time.Now()
	}
	// Capture non-streaming response body for attribute parsing
	if !w.streaming && len(w.body) < gwMaxBodyCapture {
		w.body = append(w.body, b...)
	}
	return w.ResponseWriter.Write(b)
}

func (w *genaiResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *genaiResponseWriter) interceptHeaders() {
	if v := w.Header().Get("X-Gw-Prompt-Tokens"); v != "" {
		w.promptTokens, _ = strconv.Atoi(v)
		w.Header().Del("X-Gw-Prompt-Tokens")
	}
	if v := w.Header().Get("X-Gw-Completion-Tokens"); v != "" {
		w.compTokens, _ = strconv.Atoi(v)
		w.Header().Del("X-Gw-Completion-Tokens")
	}
	if v := w.Header().Get("X-Gw-Total-Tokens"); v != "" {
		w.totalTokens, _ = strconv.Atoi(v)
		w.Header().Del("X-Gw-Total-Tokens")
	}
	if v := w.Header().Get("X-Gw-Finish-Reason"); v != "" {
		w.finishReason = v
		w.Header().Del("X-Gw-Finish-Reason")
	}
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


