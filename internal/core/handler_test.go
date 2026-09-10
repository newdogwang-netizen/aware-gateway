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
	var upstreamEpisodeHeader string
	var upstreamEpisodeOpHeader string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeader = r.Header.Get("X-Session-ID")
		upstreamEpisodeHeader = r.Header.Get("X-Episode-ID")
		upstreamEpisodeOpHeader = r.Header.Get("X-Episode-Operation")
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
			"episode_id": "episode-router-state",
			"episode_operation": "resume",
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
	if router.episodeID != "episode-router-state" {
		t.Fatalf("router episode id = %q, want episode-router-state", router.episodeID)
	}
	if router.seenEpisodeOp != "resume" {
		t.Fatalf("router episode operation header = %q, want resume", router.seenEpisodeOp)
	}
	if _, ok := router.body["session_id"]; ok {
		t.Fatalf("router body still has internal session_id: %#v", router.body)
	}
	if _, ok := router.body["episode_id"]; ok {
		t.Fatalf("router body still has internal episode_id: %#v", router.body)
	}
	if _, ok := router.body["episode_operation"]; ok {
		t.Fatalf("router body still has internal episode_operation: %#v", router.body)
	}
	if upstreamHeader != "" {
		t.Fatalf("upstream X-Session-ID = %q, want stripped", upstreamHeader)
	}
	if upstreamEpisodeHeader != "" {
		t.Fatalf("upstream X-Episode-ID = %q, want stripped", upstreamEpisodeHeader)
	}
	if upstreamEpisodeOpHeader != "" {
		t.Fatalf("upstream X-Episode-Operation = %q, want stripped", upstreamEpisodeOpHeader)
	}
	if _, ok := upstreamBody["session_id"]; ok {
		t.Fatalf("upstream body still has internal session_id: %#v", upstreamBody)
	}
	if _, ok := upstreamBody["episode_id"]; ok {
		t.Fatalf("upstream body still has internal episode_id: %#v", upstreamBody)
	}
	if _, ok := upstreamBody["episode_operation"]; ok {
		t.Fatalf("upstream body still has internal episode_operation: %#v", upstreamBody)
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
	if audit.records[0].EpisodeID != "episode-router-state" {
		t.Fatalf("audit episode id = %q, want episode-router-state", audit.records[0].EpisodeID)
	}
	if audit.records[0].EpisodeOp != "resume" {
		t.Fatalf("audit episode operation = %q, want resume", audit.records[0].EpisodeOp)
	}
}

func TestHandlerAppliesRouteBudgetToBodyAndAudit(t *testing.T) {
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
	router := &capturingRouter{
		model:        "z-ai/glm-5.3-flash",
		budgetAction: "cheap_probe",
		maxTokens:    1234,
		timeoutMs:    45000,
		episodeID:    "episode-budget",
		episodeOp:    "continue",
		stateVersion: 7,
		stateBefore:  `{"episode_id":"episode-budget","state_version":7}`,
	}
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
			"max_tokens": 9999,
			"messages": [{"role": "user", "content": "inspect files"}]
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := upstreamBody["model"]; got != "z-ai/glm-5.3-flash" {
		t.Fatalf("upstream model = %v, want z-ai/glm-5.3-flash", got)
	}
	if got := upstreamBody["max_tokens"]; got != float64(1234) {
		t.Fatalf("upstream max_tokens = %v, want 1234", got)
	}
	if len(audit.records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audit.records))
	}
	if audit.records[0].BudgetAction != "cheap_probe" {
		t.Fatalf("audit budget action = %q, want cheap_probe", audit.records[0].BudgetAction)
	}
	if audit.records[0].RouteMaxTokens != 1234 {
		t.Fatalf("audit route max tokens = %d, want 1234", audit.records[0].RouteMaxTokens)
	}
	if audit.records[0].RouteTimeoutMs != 45000 {
		t.Fatalf("audit route timeout ms = %d, want 45000", audit.records[0].RouteTimeoutMs)
	}
	if audit.records[0].EpisodeID != "episode-budget" {
		t.Fatalf("audit episode id = %q, want episode-budget", audit.records[0].EpisodeID)
	}
	if audit.records[0].EpisodeOp != "continue" {
		t.Fatalf("audit episode operation = %q, want continue", audit.records[0].EpisodeOp)
	}
	if audit.records[0].StateVersion != 7 {
		t.Fatalf("audit state version = %d, want 7", audit.records[0].StateVersion)
	}
	if audit.records[0].StateBefore != `{"episode_id":"episode-budget","state_version":7}` {
		t.Fatalf("audit state before = %q", audit.records[0].StateBefore)
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

func TestHandlerRouteTimeoutCanShortenEndpointTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"late"}}]}`)
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
	router := &capturingRouter{budgetAction: "cheap_probe", timeoutMs: 50}
	reg := plugin.NewRegistry(logger)
	if err := reg.Register(router); err != nil {
		t.Fatalf("register router: %v", err)
	}
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}

	handler := NewHandler(cfg, MapPoolProvider{"openrouter": openrouterPool}, reg, logger)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if elapsed > 180*time.Millisecond {
		t.Fatalf("handler returned after %s, want route timeout before upstream sleep completes", elapsed)
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
		"episode_id": "episode-strip",
		"episode_operation": "interrupt",
		"extra_body": {
			"session_id": "trial-strip__agent",
			"episode_id": "episode-strip",
			"episode_operation": "interrupt",
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
	if _, ok := got["episode_id"]; ok {
		t.Fatalf("top-level episode_id was not stripped: %#v", got)
	}
	if _, ok := got["episode_operation"]; ok {
		t.Fatalf("top-level episode_operation was not stripped: %#v", got)
	}
	extraBody, ok := got["extra_body"].(map[string]any)
	if !ok {
		t.Fatalf("extra_body missing or wrong type: %#v", got["extra_body"])
	}
	if _, ok := extraBody["session_id"]; ok {
		t.Fatalf("nested session_id was not stripped: %#v", extraBody)
	}
	if _, ok := extraBody["episode_id"]; ok {
		t.Fatalf("nested episode_id was not stripped: %#v", extraBody)
	}
	if _, ok := extraBody["episode_operation"]; ok {
		t.Fatalf("nested episode_operation was not stripped: %#v", extraBody)
	}
	if got := extraBody["return_token_ids"]; got != true {
		t.Fatalf("return_token_ids = %v, want true", got)
	}
}

func TestEpisodeEventEndpointIngestsAndQueriesEvents(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	store := &capturingEpisodeEventStore{}
	reg := plugin.NewRegistry(logger)
	if err := reg.Register(store); err != nil {
		t.Fatalf("register event store: %v", err)
	}
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}

	router := BuildRouter(cfg, MapPoolProvider{}, reg, logger)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/episode-events",
		bytes.NewBufferString(`{
			"kind": "test_run",
			"source": "unit-test",
			"observation": {"outcome": "passed", "command": "go test ./..."},
			"evidence_refs": ["stdout"]
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Episode-ID", "episode-api")
	req.Header.Set("X-Session-ID", "trial-api__agent")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(store.events) != 1 {
		t.Fatalf("stored events = %d, want 1", len(store.events))
	}
	if store.events[0].EventID == "" {
		t.Fatal("event id was not generated")
	}
	if store.events[0].EpisodeID != "episode-api" {
		t.Fatalf("episode id = %q, want episode-api", store.events[0].EpisodeID)
	}
	if store.events[0].SessionID != "trial-api__agent" {
		t.Fatalf("session id = %q, want trial-api__agent", store.events[0].SessionID)
	}
	if store.events[0].Timestamp.IsZero() {
		t.Fatal("timestamp was not generated")
	}

	queryReq := httptest.NewRequest(http.MethodGet, "/v1/episode-events?episode_id=episode-api&kind=test_run", nil)
	queryRec := httptest.NewRecorder()
	router.ServeHTTP(queryRec, queryReq)
	if queryRec.Code != http.StatusOK {
		t.Fatalf("query status = %d, body = %s", queryRec.Code, queryRec.Body.String())
	}
	var payload struct {
		Count  int                   `json:"count"`
		Events []plugin.EpisodeEvent `json:"events"`
	}
	if err := json.Unmarshal(queryRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode query response: %v", err)
	}
	if payload.Count != 1 || len(payload.Events) != 1 {
		t.Fatalf("query payload = %#v, want one event", payload)
	}
	if payload.Events[0].EventID != store.events[0].EventID {
		t.Fatalf("query event id = %q, want %q", payload.Events[0].EventID, store.events[0].EventID)
	}
}

func TestEpisodeStateEndpointQueriesCurrentProjection(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	store := &capturingEpisodeStateStore{
		states: []plugin.EpisodeStateEntry{
			{
				EpisodeID:    "episode-state-api",
				StateVersion: 3,
				Source:       "unit-test",
				State: map[string]any{
					"episode_id":           "episode-state-api",
					"state_version":        3,
					"test_run_count":       2,
					"no_progress_severity": "stale",
				},
			},
		},
	}
	reg := plugin.NewRegistry(logger)
	if err := reg.Register(store); err != nil {
		t.Fatalf("register state store: %v", err)
	}
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}

	router := BuildRouter(cfg, MapPoolProvider{}, reg, logger)
	req := httptest.NewRequest(http.MethodGet, "/v1/episode-state?episode_id=episode-state-api", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Count  int                        `json:"count"`
		States []plugin.EpisodeStateEntry `json:"states"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode state response: %v", err)
	}
	if payload.Count != 1 || len(payload.States) != 1 {
		t.Fatalf("state payload = %#v, want one state", payload)
	}
	state := payload.States[0]
	if state.EpisodeID != "episode-state-api" || state.StateVersion != 3 {
		t.Fatalf("state = %#v, want episode-state-api version 3", state)
	}
	if got := state.State["no_progress_severity"]; got != "stale" {
		t.Fatalf("no_progress_severity = %#v, want stale", got)
	}
}

func TestEpisodeSessionEndpointQueriesCurrentStack(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	store := &capturingEpisodeSessionStore{
		sessions: []plugin.EpisodeSessionEntry{
			{
				SessionID:       "session-api",
				ActiveEpisodeID: "session-api#episode-1",
				EpisodeStack:    []string{"session-api", "session-api#episode-1"},
				StackDepth:      2,
				NextEpisode:     1,
				Version:         2,
				LastOperation:   "interrupt",
				LastConfidence:  0.86,
				LastEvidence:    []string{"detected_side_task_language"},
				Source:          "unit-test",
			},
		},
	}
	reg := plugin.NewRegistry(logger)
	if err := reg.Register(store); err != nil {
		t.Fatalf("register session store: %v", err)
	}
	if err := reg.Init(&plugin.Context{Config: cfg, Logger: logger}); err != nil {
		t.Fatalf("init registry: %v", err)
	}

	router := BuildRouter(cfg, MapPoolProvider{}, reg, logger)
	req := httptest.NewRequest(http.MethodGet, "/v1/episode-sessions?session_id=session-api", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Count    int                          `json:"count"`
		Sessions []plugin.EpisodeSessionEntry `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	if payload.Count != 1 || len(payload.Sessions) != 1 {
		t.Fatalf("session payload = %#v, want one session", payload)
	}
	session := payload.Sessions[0]
	if session.ActiveEpisodeID != "session-api#episode-1" || session.StackDepth != 2 {
		t.Fatalf("session = %#v, want branch active with depth 2", session)
	}
	if session.LastOperation != "interrupt" {
		t.Fatalf("last operation = %q, want interrupt", session.LastOperation)
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
	sessionID     string
	trialName     string
	episodeID     string
	body          map[string]any
	model         string
	budgetAction  string
	maxTokens     int
	timeoutMs     int
	episodeOp     string
	seenEpisodeOp string
	stateVersion  int
	stateBefore   string
}

func (r *capturingRouter) Name() string { return "capturing-router" }

func (r *capturingRouter) Init(*plugin.Context) error { return nil }

func (r *capturingRouter) Close() error { return nil }

func (r *capturingRouter) Route(req *http.Request, body []byte) (*plugin.RoutingDecision, error) {
	r.sessionID = req.Header.Get("X-Session-ID")
	r.trialName = req.Header.Get("X-Trial-Name")
	if headerEpisodeID := req.Header.Get("X-Episode-ID"); headerEpisodeID != "" {
		r.episodeID = headerEpisodeID
	}
	r.seenEpisodeOp = req.Header.Get("X-Episode-Operation")
	_ = json.Unmarshal(body, &r.body)
	model := r.model
	if model == "" {
		model = "openai/gpt-5.6-sol"
	}
	episodeID := r.episodeID
	return &plugin.RoutingDecision{
		Pool:                "openrouter",
		Model:               model,
		Reason:              "test route",
		BudgetAction:        r.budgetAction,
		MaxTokens:           r.maxTokens,
		TimeoutMs:           r.timeoutMs,
		EpisodeID:           episodeID,
		EpisodeOperation:    r.episodeOp,
		EpisodeStateVersion: r.stateVersion,
		EpisodeStateBefore:  r.stateBefore,
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

type capturingEpisodeEventStore struct {
	events []plugin.EpisodeEvent
}

func (s *capturingEpisodeEventStore) Name() string { return "capturing-episode-events" }

func (s *capturingEpisodeEventStore) Init(*plugin.Context) error { return nil }

func (s *capturingEpisodeEventStore) Close() error { return nil }

func (s *capturingEpisodeEventStore) RecordEpisodeEvent(event *plugin.EpisodeEvent) error {
	copyEvent := *event
	if event.Observation != nil {
		copyEvent.Observation = make(map[string]any, len(event.Observation))
		for key, value := range event.Observation {
			copyEvent.Observation[key] = value
		}
	}
	copyEvent.EvidenceRefs = append([]string(nil), event.EvidenceRefs...)
	s.events = append(s.events, copyEvent)
	return nil
}

func (s *capturingEpisodeEventStore) QueryEpisodeEvents(filter plugin.EpisodeEventFilter) ([]plugin.EpisodeEvent, error) {
	var out []plugin.EpisodeEvent
	for _, event := range s.events {
		if filter.EpisodeID != "" && event.EpisodeID != filter.EpisodeID {
			continue
		}
		if filter.Kind != "" && event.Kind != filter.Kind {
			continue
		}
		out = append(out, event)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

type capturingEpisodeStateStore struct {
	states []plugin.EpisodeStateEntry
}

func (s *capturingEpisodeStateStore) Name() string { return "capturing-episode-state" }

func (s *capturingEpisodeStateStore) Init(*plugin.Context) error { return nil }

func (s *capturingEpisodeStateStore) Close() error { return nil }

func (s *capturingEpisodeStateStore) QueryEpisodeStates(filter plugin.EpisodeStateFilter) ([]plugin.EpisodeStateEntry, error) {
	var out []plugin.EpisodeStateEntry
	for _, state := range s.states {
		if filter.EpisodeID != "" && state.EpisodeID != filter.EpisodeID {
			continue
		}
		out = append(out, state)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

type capturingEpisodeSessionStore struct {
	sessions []plugin.EpisodeSessionEntry
}

func (s *capturingEpisodeSessionStore) Name() string { return "capturing-episode-session" }

func (s *capturingEpisodeSessionStore) Init(*plugin.Context) error { return nil }

func (s *capturingEpisodeSessionStore) Close() error { return nil }

func (s *capturingEpisodeSessionStore) QueryEpisodeSessions(filter plugin.EpisodeSessionFilter) ([]plugin.EpisodeSessionEntry, error) {
	var out []plugin.EpisodeSessionEntry
	for _, session := range s.sessions {
		if filter.SessionID != "" && session.SessionID != filter.SessionID {
			continue
		}
		out = append(out, session)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}
