package core

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/plugin"
	"github.com/aware/gateway/internal/pool"
)

func TestHandlerBodySessionIDFeedsRouterAuditAndIsStrippedUpstream(t *testing.T) {
	var upstreamHeader string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeader = r.Header.Get("X-Session-ID")
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"finish_reason": "stop", "message": {"content": "ok"}}],
			"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1},
		Routes: []config.RouteConfig{
			{Pattern: "/v1/chat/completions", Pool: "openrouter"},
		},
	}
	openrouterPool, err := pool.NewPool("openrouter", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{Name: "upstream", URL: upstream.URL, Weight: 1, Timeout: time.Second},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := &capturingRouter{}
	audit := &capturingAuditSink{}
	reg := plugin.NewRegistry(logger)
	for _, p := range []plugin.Plugin{router, audit} {
		if err := reg.Register(p); err != nil {
			t.Fatalf("register plugin %s: %v", p.Name(), err)
		}
	}
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}

	handler := NewHandler(cfg, MapPoolProvider{"openrouter": openrouterPool}, reg, logger)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(`{
			"model": "auto",
			"session_id": "trial-abc__agent",
			"messages": [{"role": "user", "content": "fix the failing tests"}]
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if router.sessionID != "trial-abc__agent" {
		t.Fatalf("router session id = %q, want trial-abc__agent", router.sessionID)
	}
	if router.trialName != "trial-abc" {
		t.Fatalf("router trial name = %q, want trial-abc", router.trialName)
	}
	if _, ok := router.body["session_id"]; ok {
		t.Fatalf("router body still has internal session_id: %#v", router.body)
	}
	if upstreamHeader != "" {
		t.Fatalf("upstream X-Session-ID = %q, want stripped", upstreamHeader)
	}
	if _, ok := upstreamBody["session_id"]; ok {
		t.Fatalf("upstream body still has internal session_id: %#v", upstreamBody)
	}
	if got := upstreamBody["model"]; got != "openai/gpt-5.6-sol" {
		t.Fatalf("upstream model = %v, want openai/gpt-5.6-sol", got)
	}
	if len(audit.records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audit.records))
	}
	if audit.records[0].SessionID != "trial-abc__agent" {
		t.Fatalf("audit session id = %q, want trial-abc__agent", audit.records[0].SessionID)
	}
	if audit.records[0].TrialName != "trial-abc" {
		t.Fatalf("audit trial name = %q, want trial-abc", audit.records[0].TrialName)
	}
}

func TestHandlerForcesIdentityEncodingSoUsageCanBeAudited(t *testing.T) {
	var upstreamAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"finish_reason": "stop", "message": {"content": "ok"}}],
			"usage": {
				"prompt_tokens": 7,
				"completion_tokens": 3,
				"total_tokens": 10,
				"cost": 0,
				"cost_details": {"upstream_inference_cost": 0.0123}
			}
		}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1},
		Routes: []config.RouteConfig{
			{Pattern: "/v1/chat/completions", Pool: "openrouter"},
		},
	}
	openrouterPool, err := pool.NewPool("openrouter", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{Name: "upstream", URL: upstream.URL, Weight: 1, Timeout: time.Second},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	audit := &capturingAuditSink{}
	reg := plugin.NewRegistry(logger)
	if err := reg.Register(audit); err != nil {
		t.Fatalf("register audit plugin: %v", err)
	}
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}

	handler := NewHandler(cfg, MapPoolProvider{"openrouter": openrouterPool}, reg, logger)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"openai/auto","messages":[{"role":"user","content":"hi"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if upstreamAcceptEncoding != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q, want identity", upstreamAcceptEncoding)
	}
	if len(audit.records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audit.records))
	}
	got := audit.records[0]
	if got.PromptTokens != 7 || got.CompTokens != 3 || got.TotalTokens != 10 {
		t.Fatalf("audit tokens = %d/%d/%d, want 7/3/10", got.PromptTokens, got.CompTokens, got.TotalTokens)
	}
	if got.Cost != 0.0123 {
		t.Fatalf("audit cost = %v, want 0.0123", got.Cost)
	}
}

func TestSessionIDFromNestedExtraBody(t *testing.T) {
	body := []byte(`{
		"model": "auto",
		"extra_body": {"session_id": "trial-nested__agent"}
	}`)

	if got := sessionIDFromBody(body); got != "trial-nested__agent" {
		t.Fatalf("sessionIDFromBody = %q, want trial-nested__agent", got)
	}
}

func TestTrialNameFromHarborSessionID(t *testing.T) {
	got := trialNameFromSessionID("html-js-filter__HFeo4ds__agent")
	if got != "html-js-filter__HFeo4ds" {
		t.Fatalf("trialNameFromSessionID = %q, want html-js-filter__HFeo4ds", got)
	}
}

func TestNormalizeModelFieldTrimsAndRewritesBody(t *testing.T) {
	body := []byte("{\"model\":\"openai/gpt-5.6-sol\\r\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")

	normalized, model := normalizeModelField(body, "application/json")
	if model != "openai/gpt-5.6-sol" {
		t.Fatalf("model = %q, want openai/gpt-5.6-sol", model)
	}
	var got map[string]any
	if err := json.Unmarshal(normalized, &got); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	if got["model"] != "openai/gpt-5.6-sol" {
		t.Fatalf("body model = %q, want openai/gpt-5.6-sol", got["model"])
	}
}

func TestHandlerModelMapCanonicalizesLiteLLMFlashPrefix(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"finish_reason": "stop", "message": {"content": "ok"}}],
			"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1},
		Routes: []config.RouteConfig{
			{Pattern: "/v1/chat/completions", Pool: "openrouter"},
		},
		ModelMap: map[string]string{
			"openai/z-ai/glm-5.3-flash": "z-ai/glm-5.3-flash",
		},
	}
	openrouterPool, err := pool.NewPool("openrouter", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{Name: "upstream", URL: upstream.URL, Weight: 1, Timeout: time.Second},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := plugin.NewRegistry(logger)
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}
	handler := NewHandler(cfg, MapPoolProvider{"openrouter": openrouterPool}, reg, logger)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"openai/z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if upstreamBody["model"] != "z-ai/glm-5.3-flash" {
		t.Fatalf("upstream model = %q, want z-ai/glm-5.3-flash", upstreamBody["model"])
	}
	reasoning, ok := upstreamBody["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning missing or wrong type: %#v", upstreamBody["reasoning"])
	}
	if reasoning["effort"] != "low" || reasoning["exclude"] != true {
		t.Fatalf("reasoning = %#v, want low/exclude", reasoning)
	}
}

func TestStripInternalRequestFieldsKeepsOtherExtraBodyFields(t *testing.T) {
	body := []byte(`{
		"model": "auto",
		"session_id": "trial-strip__agent",
		"extra_body": {
			"session_id": "trial-strip__agent",
			"return_token_ids": true
		}
	}`)

	stripped := stripInternalRequestFields(body)
	var got map[string]any
	if err := json.Unmarshal(stripped, &got); err != nil {
		t.Fatalf("decode stripped body: %v", err)
	}
	if _, ok := got["session_id"]; ok {
		t.Fatalf("top-level session_id was not stripped: %#v", got)
	}
	extraBody, ok := got["extra_body"].(map[string]any)
	if !ok {
		t.Fatalf("extra_body missing or wrong type: %#v", got["extra_body"])
	}
	if _, ok := extraBody["session_id"]; ok {
		t.Fatalf("nested session_id was not stripped: %#v", extraBody)
	}
	if got := extraBody["return_token_ids"]; got != true {
		t.Fatalf("return_token_ids = %v, want true", got)
	}
}

func TestEnsureStreamUsageAddsIncludeUsage(t *testing.T) {
	body := []byte(`{
		"model": "auto",
		"stream": true,
		"stream_options": {"existing": "kept"}
	}`)

	rewritten := ensureStreamUsage(body, "application/json")
	var got map[string]any
	if err := json.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	streamOptions, ok := got["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing or wrong type: %#v", got["stream_options"])
	}
	if streamOptions["include_usage"] != true {
		t.Fatalf("include_usage = %v, want true", streamOptions["include_usage"])
	}
	if streamOptions["existing"] != "kept" {
		t.Fatalf("existing stream option = %v, want kept", streamOptions["existing"])
	}
}

func TestEnsureStreamUsageLeavesNonStreamingBodyUnchanged(t *testing.T) {
	body := []byte(`{"model":"auto","messages":[]}`)

	rewritten := ensureStreamUsage(body, "application/json")
	if string(rewritten) != string(body) {
		t.Fatalf("body changed for non-streaming request: %s", rewritten)
	}
}

func TestEnsureFlashReasoningLowAddsReasoningForFlash(t *testing.T) {
	body := []byte(`{"model":"z-ai/glm-5.3-flash","messages":[]}`)

	rewritten := ensureFlashReasoningLow(body, "application/json", "z-ai/glm-5.3-flash")
	var got map[string]any
	if err := json.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning missing or wrong type: %#v", got["reasoning"])
	}
	if reasoning["effort"] != "low" {
		t.Fatalf("reasoning effort = %v, want low", reasoning["effort"])
	}
	if reasoning["exclude"] != true {
		t.Fatalf("reasoning exclude = %v, want true", reasoning["exclude"])
	}
}

func TestEnsureFlashReasoningLowPreservesExplicitReasoning(t *testing.T) {
	body := []byte(`{"model":"z-ai/glm-5.3-flash","reasoning":{"effort":"high"},"messages":[]}`)

	rewritten := ensureFlashReasoningLow(body, "application/json", "z-ai/glm-5.3-flash")
	var got map[string]any
	if err := json.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	reasoning := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning effort = %v, want high", reasoning["effort"])
	}
}

func TestEnsureFlashReasoningLowLeavesOtherModelsUnchanged(t *testing.T) {
	body := []byte(`{"model":"openai/gpt-5.6-sol","messages":[]}`)

	rewritten := ensureFlashReasoningLow(body, "application/json", "openai/gpt-5.6-sol")
	if string(rewritten) != string(body) {
		t.Fatalf("body changed for non-flash model: %s", rewritten)
	}
}

func TestDecisionWriterCapturesStreamingUsageTail(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &decisionWriter{
		real:      rec,
		retryable: map[int]bool{},
		header:    make(http.Header),
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`data: {"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n"))
	if err != nil {
		t.Fatalf("write stream chunk: %v", err)
	}

	if !w.streaming {
		t.Fatal("writer did not detect streaming response")
	}
	prompt, completion, total, ok := parseSSEUsage(w.tail)
	if !ok {
		t.Fatalf("parseSSEUsage did not find usage in %q", string(w.tail))
	}
	if prompt != 11 || completion != 7 || total != 18 {
		t.Fatalf("usage = %d/%d/%d, want 11/7/18", prompt, completion, total)
	}
}

func TestHandlerReleasesInFlightOnNonStreamingBodyTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"partial`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Retry: config.RetryConfig{MaxRetries: 1},
		Routes: []config.RouteConfig{
			{Pattern: "/v1/chat/completions", Pool: "openrouter"},
		},
	}
	openrouterPool, err := pool.NewPool("openrouter", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{Name: "upstream", URL: upstream.URL, Weight: 1, Timeout: 50 * time.Millisecond},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := plugin.NewRegistry(logger)
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}
	handler := NewHandler(cfg, MapPoolProvider{"openrouter": openrouterPool}, reg, logger)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if got := openrouterPool.Endpoints[0].InFlight(); got != 0 {
		t.Fatalf("in-flight = %d, want 0", got)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("handler returned after %s, want endpoint timeout to end attempt promptly", elapsed)
	}
}

type capturingRouter struct {
	sessionID string
	trialName string
	body      map[string]any
}

func (r *capturingRouter) Name() string { return "capturing-router" }

func (r *capturingRouter) Init(*plugin.Context) error { return nil }

func (r *capturingRouter) Close() error { return nil }

func (r *capturingRouter) Route(req *http.Request, body []byte) (*plugin.RoutingDecision, error) {
	r.sessionID = req.Header.Get("X-Session-ID")
	r.trialName = req.Header.Get("X-Trial-Name")
	_ = json.Unmarshal(body, &r.body)
	return &plugin.RoutingDecision{
		Pool:   "openrouter",
		Model:  "openai/gpt-5.6-sol",
		Reason: "test route",
	}, nil
}

type capturingAuditSink struct {
	records []*plugin.AuditRecord
}

func (s *capturingAuditSink) Name() string { return "capturing-audit" }

func (s *capturingAuditSink) Init(*plugin.Context) error { return nil }

func (s *capturingAuditSink) Close() error { return nil }

func (s *capturingAuditSink) Record(record *plugin.AuditRecord) error {
	copyRecord := *record
	s.records = append(s.records, &copyRecord)
	return nil
}
