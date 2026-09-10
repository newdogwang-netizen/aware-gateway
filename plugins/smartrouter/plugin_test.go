package smartrouter

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/plugin"
)

func TestRouteFallsBackToConfiguredModelWhenDecisionModelFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "decision unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"fix failing tests"}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want configured fallback decision")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("fallback model = %q, want anthropic/claude-opus-5", decision.Model)
	}
	if decision.Pool != "openrouter" {
		t.Fatalf("fallback pool = %q, want openrouter", decision.Pool)
	}
	if !strings.Contains(decision.Reason, "decision-model-error") {
		t.Fatalf("fallback reason = %q, want decision-model-error", decision.Reason)
	}
}

func TestRouteRetriesTransientDecisionModelFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "temporary decision failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"reason\":\"recovered\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.DecisionRetries = 1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"inspect environment"}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want recovered decision")
	}
	if decision.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("model = %q, want z-ai/glm-5.3-flash", decision.Model)
	}
	if calls != 2 {
		t.Fatalf("decision server calls = %d, want 2", calls)
	}
}

func TestRouteRetriesMalformedDecisionJSON(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = io.WriteString(w, `{
				"choices": [{"message": {"content": "{\"model\":\"anthropic/cla"}}],
				"usage": {"prompt_tokens": 10, "completion_tokens": 200, "total_tokens": 210}
			}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"anthropic/claude-opus-5\",\"reason\":\"high leverage\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.DecisionRetries = 1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"implement feature"}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want recovered decision")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want anthropic/claude-opus-5", decision.Model)
	}
	if strings.Contains(decision.Reason, "fallback") {
		t.Fatalf("reason = %q, want decision route not fallback", decision.Reason)
	}
	if calls != 2 {
		t.Fatalf("decision server calls = %d, want 2", calls)
	}
}

func TestRouteFallsBackToConfiguredModelWhenDecisionModelReturnsUnknownModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"openai/gpt-5.6-luna\",\"reason\":\"cheap\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"implement feature"}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want configured fallback decision")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("fallback model = %q, want anthropic/claude-opus-5", decision.Model)
	}
	if !strings.Contains(decision.Reason, "unknown-model") {
		t.Fatalf("fallback reason = %q, want unknown-model", decision.Reason)
	}
}

func TestRouteAcceptsDecisionModelProviderPrefixAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"openai/anthropic/claude-opus-5\",\"reason\":\"canonicalize provider prefix\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"fix failing tests"}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want canonicalized model decision")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want canonical Opus model", decision.Model)
	}
}

func TestDecisionModelRequestUsesAuthAndOmitsThinkingParamByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want Bearer test-key", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		if strings.Contains(string(raw), "chat_template_kwargs") {
			t.Fatalf("request body unexpectedly included provider-specific thinking param: %s", string(raw))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"reason\":\"cheap\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.APIKey = "test-key"
	router.cfg.Model = "openai/gpt-5.6-sol"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	decision, err := router.callDecisionModel("pick a model", req)
	if err != nil {
		t.Fatalf("callDecisionModel returned error: %v", err)
	}
	if decision.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("decision model = %q, want flash", decision.Model)
	}
}

func TestRouteSkipsPinnedModelWithTrailingCR(t *testing.T) {
	router := newTestSmartRouter("http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte("{\"model\":\"anthropic/claude-opus-5\\r\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}")

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || !decision.Skip {
		t.Fatalf("Route = %#v, want skip for pinned Opus model", decision)
	}
}

func TestRouteSkipsPinnedModelWithLiteLLMOpenAIProviderPrefix(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"openai/z-ai/glm-5.3-flash","messages":[{"role":"user","content":"hello"}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want skip for explicit flash model")
	}
	if decision == nil || !decision.Skip {
		t.Fatalf("Route = %#v, want skip for explicit flash model", decision)
	}
}

func TestRouteSkipsPinnedAnthropicModelWithLiteLLMOpenAIProviderPrefix(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"openai/anthropic/claude-opus-5","messages":[{"role":"user","content":"hello"}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want skip for explicit Opus model")
	}
	if decision == nil || !decision.Skip {
		t.Fatalf("Route = %#v, want skip for explicit Opus model", decision)
	}
}

func TestWarmStartRoutesFirstNCallsBySessionThenUsesDecisionModel(t *testing.T) {
	decisionServerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"reason\":\"cheap after warm start\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.WarmStart = WarmStartConfig{
		TriggerModels: []string{"auto-opus-warmstart"},
		Steps:         5,
		Model:         "anthropic/claude-opus-5",
		Pool:          "openrouter",
	}
	router.warmCounts = map[string]int{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "trial-1__agent")
	body := []byte(`{"model":"auto-opus-warmstart","messages":[{"role":"user","content":"work"}]}`)

	for i := 1; i <= 5; i++ {
		decision, err := router.Route(req, body)
		if err != nil {
			t.Fatalf("Route call %d returned error: %v", i, err)
		}
		if decision == nil || decision.Skip {
			t.Fatalf("Route call %d skipped; want warm-start Opus decision", i)
		}
		if decision.Model != "anthropic/claude-opus-5" {
			t.Fatalf("Route call %d model = %q, want Opus", i, decision.Model)
		}
		if !strings.Contains(decision.Reason, "warm-start") {
			t.Fatalf("Route call %d reason = %q, want warm-start", i, decision.Reason)
		}
	}

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route call 6 returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route call 6 skipped; want decision-model route")
	}
	if decision.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("Route call 6 model = %q, want flash", decision.Model)
	}
	if decisionServerCalls != 1 {
		t.Fatalf("decision server calls = %d, want 1", decisionServerCalls)
	}

	third, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("third Route returned error: %v", err)
	}
	if third == nil || third.Skip {
		t.Fatalf("third Route skipped; want decision-model route after one local escalation")
	}
	if strings.Contains(third.Reason, "rule_id=repeated_error_upgrade") {
		t.Fatalf("third reason = %q, repeated-error rule should not auto-upgrade same fingerprint twice", third.Reason)
	}
	if decisionServerCalls != 2 {
		t.Fatalf("decision server calls = %d, want 2 after third fallthrough", decisionServerCalls)
	}
}

func TestSafeControlErrorFingerprintPrefersSpecificTracebackLine(t *testing.T) {
	got := safeControlErrorFingerprint("Traceback (most recent call last):\n  File \"solver.py\", line 10, in <module>\nAssertionError: expected relay id 7 got 8")
	if strings.Contains(got, "traceback") {
		t.Fatalf("fingerprint = %q, want specific error line instead of generic traceback header", got)
	}
	if !strings.Contains(got, "assertionerror") {
		t.Fatalf("fingerprint = %q, want assertion error detail", got)
	}
}

func TestSafeControlDoesNotTreatImplementationProtocolAsFixedFormatOnly(t *testing.T) {
	decisionServerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"anthropic/claude-opus-5\",\"reason\":\"implementation protocol needs semantic routing\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-fixed-format-implementation")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Respond with JSON only, then implement the parser fix by editing solver.py."}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want decision-model route")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want Opus from decision model", decision.Model)
	}
	if decisionServerCalls != 1 {
		t.Fatalf("decision server calls = %d, want 1", decisionServerCalls)
	}
	if strings.Contains(decision.Reason, "safe-control") {
		t.Fatalf("reason = %q, implementation protocol should not be a local safe-control decision", decision.Reason)
	}
}

func TestTaskCompletionConfirmationRoutesToStrongestConfiguredModel(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{
		"model": "auto",
		"messages": [
			{"role": "user", "content": "Current terminal state:\nfinal checks passed\n\nAre you sure you want to mark the task as complete? This will trigger your solution to be graded and you won't be able to make any further corrections. If so, include \"task_complete\": true in your JSON response again."}
		]
	}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want deterministic completion guardrail")
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want strongest configured model")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want anthropic/claude-opus-5", decision.Model)
	}
	if !strings.Contains(decision.Reason, "task completion confirmation") {
		t.Fatalf("reason = %q, want task completion guardrail", decision.Reason)
	}
}

func TestTaskCompletionGuardrailIncludesEpisodeReadinessEvidence(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCompletionGuardrail: {MaxTokens: 1024, TimeoutMs: 60000},
		},
	}
	for _, event := range []*plugin.EpisodeEvent{
		{
			EventID:   "event-delivery-file",
			EpisodeID: "episode-completion-ready",
			Timestamp: time.Now(),
			Kind:      "file_written",
			Source:    "unit-test",
			Observation: map[string]any{
				"target_paths":     []string{"/app/output/answer.json"},
				"delivery_target":  true,
				"workspace_target": false,
			},
		},
		{
			EventID:   "event-validation-passed",
			EpisodeID: "episode-completion-ready",
			Timestamp: time.Now(),
			Kind:      "test_run",
			Source:    "unit-test",
			Observation: map[string]any{
				"outcome":      "passed",
				"command":      "python3 validate.py",
				"passed_count": 7,
				"failed_count": 0,
			},
		},
		{
			EventID:   "event-verifier-passed",
			EpisodeID: "episode-completion-ready",
			Timestamp: time.Now(),
			Kind:      "verifier_result",
			Source:    "unit-test",
			Observation: map[string]any{
				"reward": 1.0,
			},
		},
	} {
		if err := router.RecordEpisodeEvent(event); err != nil {
			t.Fatalf("RecordEpisodeEvent %s returned error: %v", event.EventID, err)
		}
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-completion-ready"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if got := states[0].State["completion_readiness"]; got != completionReadinessVerifierPassed {
		t.Fatalf("completion readiness = %#v, want verifier_passed", got)
	}
	if got := states[0].State["delivery_file_write_count"]; got != 1 {
		t.Fatalf("delivery file writes = %#v, want 1", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Episode-ID", "episode-completion-ready")
	req.Header.Set("X-Session-ID", "episode-completion-ready")
	body := []byte(`{
		"model": "auto",
		"messages": [
			{"role": "user", "content": "Are you sure you want to mark the task as complete? Include \"task_complete\": true."}
		]
	}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want deterministic completion guardrail")
	}
	if decision == nil || decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("decision = %#v, want Opus completion guardrail", decision)
	}
	if decision.BudgetAction != budgetActionCompletionGuardrail {
		t.Fatalf("budget action = %q, want %s", decision.BudgetAction, budgetActionCompletionGuardrail)
	}
	for _, want := range []string{
		"completion_readiness=verifier_passed",
		"delivery_file_writes=1",
		"test_passed=1",
		"test_failed=0",
		"verifier_reward=1.000",
		"last_progress=verifier_result",
	} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want %q", decision.Reason, want)
		}
	}
}

func TestEpisodeCompletionReadinessRegressesAfterTargetWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCompletionGuardrail: {MaxTokens: 1024, TimeoutMs: 60000},
		},
	}

	events := []*plugin.EpisodeEvent{
		{
			EventID:   "event-delivery-file",
			EpisodeID: "episode-completion-regress",
			Timestamp: time.Now(),
			Kind:      "file_written",
			Source:    "unit-test",
			Observation: map[string]any{
				"delivery_target": true,
			},
		},
		{
			EventID:   "event-validation-passed",
			EpisodeID: "episode-completion-regress",
			Timestamp: time.Now(),
			Kind:      "test_run",
			Source:    "unit-test",
			Observation: map[string]any{
				"outcome":      "passed",
				"failed_count": 0,
			},
		},
		{
			EventID:   "event-verifier-passed",
			EpisodeID: "episode-completion-regress",
			Timestamp: time.Now(),
			Kind:      "verifier_result",
			Source:    "unit-test",
			Observation: map[string]any{
				"reward": 1.0,
			},
		},
		{
			EventID:   "event-delivery-update",
			EpisodeID: "episode-completion-regress",
			Timestamp: time.Now(),
			Kind:      "file_modified",
			Source:    "unit-test",
			Observation: map[string]any{
				"path_count":      1,
				"delivery_target": true,
			},
		},
	}
	for _, event := range events {
		if err := router.RecordEpisodeEvent(event); err != nil {
			t.Fatalf("RecordEpisodeEvent %s returned error: %v", event.EventID, err)
		}
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-completion-regress"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if got := states[0].State["completion_readiness"]; got != completionReadinessDeliveryCandidate {
		t.Fatalf("completion readiness = %#v, want delivery_candidate", got)
	}
	if got := states[0].State["delivery_file_write_count"]; got != 2 {
		t.Fatalf("delivery file writes = %#v, want 2", got)
	}
	if got := states[0].State["verifier_reward"]; got != float64(0) {
		t.Fatalf("verifier reward = %#v, want 0 after target write", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Episode-ID", "episode-completion-regress")
	req.Header.Set("X-Session-ID", "episode-completion-regress")
	body := []byte(`{
		"model": "auto",
		"messages": [
			{"role": "user", "content": "Are you sure you want to mark the task as complete? Include \"task_complete\": true."}
		]
	}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	for _, want := range []string{
		"completion_readiness=delivery_candidate",
		"delivery_file_writes=2",
		"verifier_reward=0.000",
		"last_progress=file_modified",
	} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want %q", decision.Reason, want)
		}
	}
	if strings.Contains(decision.Reason, "completion_readiness=verifier_passed") {
		t.Fatalf("reason = %q, want current readiness instead of stale verifier_passed", decision.Reason)
	}
}

func TestEpisodeCompletionReadinessMarksFailedValidation(t *testing.T) {
	router := newTestSmartRouter("http://127.0.0.1:1")
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}

	for _, event := range []*plugin.EpisodeEvent{
		{
			EventID:   "event-delivery-file",
			EpisodeID: "episode-validation-failed",
			Timestamp: time.Now(),
			Kind:      "file_written",
			Source:    "unit-test",
			Observation: map[string]any{
				"delivery_target": true,
			},
		},
		{
			EventID:   "event-validation-failed",
			EpisodeID: "episode-validation-failed",
			Timestamp: time.Now(),
			Kind:      "test_run",
			Source:    "unit-test",
			Observation: map[string]any{
				"outcome":             "failed",
				"failed_count":        2,
				"failure_fingerprint": "assert total == 42",
			},
		},
	} {
		if err := router.RecordEpisodeEvent(event); err != nil {
			t.Fatalf("RecordEpisodeEvent %s returned error: %v", event.EventID, err)
		}
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-validation-failed"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if got := states[0].State["completion_readiness"]; got != completionReadinessValidationFailed {
		t.Fatalf("completion readiness = %#v, want validation_failed", got)
	}
	if got := states[0].State["test_failed_count"]; got != 1 {
		t.Fatalf("test failed count = %#v, want 1", got)
	}
}

func TestBuildPromptIncludesCostQualityTurnRiskGuidance(t *testing.T) {
	router := newTestSmartRouter("http://127.0.0.1:1")
	prompt := router.buildPrompt(&parsedRequest{
		Model:           "auto",
		MessageCount:    4,
		EstimatedTokens: 200,
		LatestUserMsg:   "Inspect installed packages for a security-related task.",
	}, "1. model=anthropic/claude-opus-5 turn=critical_hypothesis state=forming critical=true recover=hard ctx=\"root cause unclear\" reason=\"need path setting\"", "")

	for _, want := range []string{
		"Optimize for final task quality per dollar, not for speed.",
		"Recent router memory for this same trial",
		"root cause unclear",
		"Do not upgrade solely because the task domain mentions security",
		"Do not treat all validation as cheap",
		"Use recent memory as routing evidence",
		"Use the strongest model for finalization or submission turns after local checks pass",
		"Do not upgrade merely because the terminal agent must reply in JSON",
		"Prefer the cheaper model when it can safely advance the task",
		"Budget actions:",
		"budget_action",
		"context_summary",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRouteFeedsEpisodeStateIntoNextPrompt(t *testing.T) {
	var prompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode decision request: %v", err)
		}
		if len(payload.Messages) == 0 {
			t.Fatal("decision request had no messages")
		}
		prompt = payload.Messages[0].Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"anthropic/claude-opus-5\",\"turn_type\":\"recovery\",\"hypothesis_state\":\"contradicted\",\"critical_path\":true,\"recoverability\":\"hard\",\"budget_action\":\"premium_recover\",\"context_summary\":\"length truncation blocked progress\",\"reason\":\"restore complete context\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "episode-prompt")
	if err := router.Record(&plugin.AuditRecord{
		TraceID:      "trace-episode-prompt-1",
		Timestamp:    time.Now(),
		SessionID:    "episode-prompt",
		Pool:         "openrouter",
		RoutedModel:  "z-ai/glm-5.3-flash",
		Status:       200,
		FinishReason: "length",
		BudgetAction: budgetActionCheapExecute,
		TotalTokens:  4096,
		Cost:         0.25,
		LatencyMs:    60000,
	}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue implementing the parser."}]}`)
	if _, err := router.Route(req, body); err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	for _, want := range []string{
		"Episode state projected from previous agent calls",
		"length_streak=1",
		"recent_length=1",
		"route_outcome trace=trace-episode-prompt-1 label=pending",
		"outcome=length_truncated",
		"finish_reason=length",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRouteAnnotatesEpisodeStateBeforeAndAfter(t *testing.T) {
	var prompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode decision request: %v", err)
		}
		if len(payload.Messages) == 0 {
			t.Fatal("decision request had no messages")
		}
		prompt = payload.Messages[0].Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"turn_type\":\"mechanical_probe\",\"hypothesis_state\":\"stable\",\"critical_path\":false,\"recoverability\":\"easy\",\"budget_action\":\"cheap_probe\",\"context_summary\":\"bounded check\",\"reason\":\"cheap probe\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}
	initial := &plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "session-main",
		EpisodeID:    "episode-main",
		Pool:         "openrouter",
		RoutedModel:  "anthropic/claude-opus-5",
		Status:       200,
		FinishReason: "length",
		BudgetAction: budgetActionPremiumReason,
		TotalTokens:  1000,
		Cost:         0.25,
	}
	if err := router.Record(initial); err != nil {
		t.Fatalf("initial Record returned error: %v", err)
	}
	if !strings.Contains(initial.StateAfter, `"state_version":1`) {
		t.Fatalf("initial state after = %q, want state_version 1", initial.StateAfter)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "session-main")
	req.Header.Set("X-Episode-ID", "episode-main")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Inspect the latest output and decide the next narrow check."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route skipped; want routed decision")
	}
	if decision.EpisodeID != "episode-main" {
		t.Fatalf("episode id = %q, want episode-main", decision.EpisodeID)
	}
	if decision.EpisodeOperation != "continue" {
		t.Fatalf("episode operation = %q, want continue", decision.EpisodeOperation)
	}
	if decision.EpisodeStateVersion != 1 {
		t.Fatalf("episode state version = %d, want 1", decision.EpisodeStateVersion)
	}
	if !strings.Contains(decision.EpisodeStateBefore, `"episode_id":"episode-main"`) ||
		!strings.Contains(decision.EpisodeStateBefore, `"state_version":1`) {
		t.Fatalf("episode state before = %q", decision.EpisodeStateBefore)
	}
	if !strings.Contains(prompt, "episode_id=episode-main state_version=1") {
		t.Fatalf("decision prompt missing episode state version:\n%s", prompt)
	}

	finished := &plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "session-main",
		EpisodeID:    decision.EpisodeID,
		EpisodeOp:    decision.EpisodeOperation,
		StateVersion: decision.EpisodeStateVersion,
		StateBefore:  decision.EpisodeStateBefore,
		Pool:         "openrouter",
		RoutedModel:  decision.Model,
		Status:       200,
		FinishReason: "stop",
		BudgetAction: decision.BudgetAction,
		TotalTokens:  400,
		Cost:         0.01,
	}
	if err := router.Record(finished); err != nil {
		t.Fatalf("finished Record returned error: %v", err)
	}
	if !strings.Contains(finished.StateAfter, `"state_version":2`) {
		t.Fatalf("finished state after = %q, want state_version 2", finished.StateAfter)
	}
}

func TestEpisodeLinksPostedOutcomesToPreviousRoute(t *testing.T) {
	router := newTestSmartRouter("http://127.0.0.1:1")
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 8}

	firstRoute := &plugin.AuditRecord{
		TraceID:      "trace-route-1",
		Timestamp:    time.Now(),
		SessionID:    "session-route-outcome",
		EpisodeID:    "episode-route-outcome",
		Pool:         "openrouter",
		RoutedModel:  "z-ai/glm-5.3-flash",
		Status:       200,
		FinishReason: "stop",
		BudgetAction: budgetActionCheapExecute,
		TotalTokens:  300,
		Cost:         0.01,
	}
	if err := router.Record(firstRoute); err != nil {
		t.Fatalf("Record first route returned error: %v", err)
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-route-outcome"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates after first route returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if got := states[0].State["last_route_trace_id"]; got != "trace-route-1" {
		t.Fatalf("last route trace = %#v, want trace-route-1", got)
	}
	if got := states[0].State["last_route_outcome_label"]; got != routeOutcomePending {
		t.Fatalf("route outcome label = %#v, want pending", got)
	}

	for _, event := range []*plugin.EpisodeEvent{
		{
			EventID:   "event-delivery",
			EpisodeID: "episode-route-outcome",
			Timestamp: time.Now(),
			Kind:      "file_written",
			Source:    "unit-test",
			Observation: map[string]any{
				"delivery_target": true,
			},
		},
		{
			EventID:   "event-test-failed",
			EpisodeID: "episode-route-outcome",
			Timestamp: time.Now(),
			Kind:      "test_run",
			Source:    "unit-test",
			Observation: map[string]any{
				"outcome":             "failed",
				"failed_count":        1,
				"failure_fingerprint": "assert route outcome",
			},
		},
		{
			EventID:   "event-test-passed",
			EpisodeID: "episode-route-outcome",
			Timestamp: time.Now(),
			Kind:      "test_run",
			Source:    "unit-test",
			Observation: map[string]any{
				"outcome":      "passed",
				"failed_count": 0,
			},
		},
	} {
		if err := router.RecordEpisodeEvent(event); err != nil {
			t.Fatalf("RecordEpisodeEvent %s returned error: %v", event.EventID, err)
		}
	}

	states, err = router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-route-outcome"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates after outcomes returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	state := states[0].State
	if got := state["last_route_trace_id"]; got != "trace-route-1" {
		t.Fatalf("last route trace = %#v, want trace-route-1", got)
	}
	if got := state["last_route_outcome_label"]; got != routeOutcomeTestPassed {
		t.Fatalf("route outcome label = %#v, want test_passed", got)
	}
	if got := state["last_route_outcome_event_id"]; got != "event-test-passed" {
		t.Fatalf("last route outcome event = %#v, want event-test-passed", got)
	}
	if got := state["last_route_outcome_progress"]; got != true {
		t.Fatalf("last route outcome progress = %#v, want true", got)
	}
	if got := state["last_route_outcome_event_count"]; got != 3 {
		t.Fatalf("last route outcome event count = %#v, want 3", got)
	}
	if got := state["route_outcome_event_count"]; got != 3 {
		t.Fatalf("route outcome event count = %#v, want 3", got)
	}
	if got := state["route_outcome_progress_count"]; got != 2 {
		t.Fatalf("route outcome progress count = %#v, want 2", got)
	}
	if got := state["route_outcome_negative_count"]; got != 1 {
		t.Fatalf("route outcome negative count = %#v, want 1", got)
	}

	secondRoute := &plugin.AuditRecord{
		TraceID:      "trace-route-2",
		Timestamp:    time.Now(),
		SessionID:    "session-route-outcome",
		EpisodeID:    "episode-route-outcome",
		Pool:         "openrouter",
		RoutedModel:  "anthropic/claude-opus-5",
		Status:       200,
		FinishReason: "stop",
		BudgetAction: budgetActionPremiumRecover,
		TotalTokens:  700,
		Cost:         0.08,
	}
	if err := router.Record(secondRoute); err != nil {
		t.Fatalf("Record second route returned error: %v", err)
	}
	states, err = router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-route-outcome"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates after second route returned error: %v", err)
	}
	state = states[0].State
	if got := state["last_route_trace_id"]; got != "trace-route-2" {
		t.Fatalf("last route trace = %#v, want trace-route-2", got)
	}
	if got := state["last_route_outcome_label"]; got != routeOutcomePending {
		t.Fatalf("route outcome label = %#v, want pending after new route", got)
	}
	if got := state["last_route_outcome_event_count"]; got != 0 {
		t.Fatalf("last route outcome event count = %#v, want reset to 0", got)
	}
	if got := state["route_outcome_event_count"]; got != 3 {
		t.Fatalf("total route outcome events = %#v, want total preserved", got)
	}
}

func TestEpisodeResolverInterruptsAndResumesTaskLines(t *testing.T) {
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode decision request: %v", err)
		}
		if len(payload.Messages) == 0 {
			t.Fatal("decision request had no messages")
		}
		prompts = append(prompts, payload.Messages[0].Content)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"turn_type\":\"mechanical_probe\",\"hypothesis_state\":\"stable\",\"critical_path\":false,\"recoverability\":\"easy\",\"budget_action\":\"cheap_probe\",\"context_summary\":\"bounded check\",\"reason\":\"cheap probe\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}

	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req1.Header.Set("X-Session-ID", "session-stack")
	body1 := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue main implementation."}]}`)
	decision1, err := router.Route(req1, body1)
	if err != nil {
		t.Fatalf("first Route returned error: %v", err)
	}
	if decision1.EpisodeID != "session-stack" {
		t.Fatalf("first episode id = %q, want session-stack", decision1.EpisodeID)
	}
	if decision1.EpisodeOperation != "continue" {
		t.Fatalf("first episode operation = %q, want continue", decision1.EpisodeOperation)
	}
	mainRecord := &plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "session-stack",
		EpisodeID:    decision1.EpisodeID,
		EpisodeOp:    decision1.EpisodeOperation,
		StateVersion: decision1.EpisodeStateVersion,
		StateBefore:  decision1.EpisodeStateBefore,
		Pool:         "openrouter",
		RoutedModel:  decision1.Model,
		Status:       200,
		FinishReason: "stop",
		BudgetAction: decision1.BudgetAction,
		TotalTokens:  400,
		Cost:         0.01,
	}
	if err := router.Record(mainRecord); err != nil {
		t.Fatalf("main Record returned error: %v", err)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set("X-Session-ID", "session-stack")
	body2 := []byte(`{"model":"auto","messages":[{"role":"user","content":"Before that, handle a separate task about OpenRouter 502."}]}`)
	decision2, err := router.Route(req2, body2)
	if err != nil {
		t.Fatalf("interrupt Route returned error: %v", err)
	}
	if decision2.EpisodeID != "session-stack#episode-1" {
		t.Fatalf("interrupt episode id = %q, want session-stack#episode-1", decision2.EpisodeID)
	}
	if decision2.EpisodeOperation != "interrupt" {
		t.Fatalf("interrupt episode operation = %q, want interrupt", decision2.EpisodeOperation)
	}
	if decision2.EpisodeStateVersion != 0 {
		t.Fatalf("interrupt state version = %d, want isolated empty state", decision2.EpisodeStateVersion)
	}
	branchRecord := &plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "session-stack",
		EpisodeID:    decision2.EpisodeID,
		EpisodeOp:    decision2.EpisodeOperation,
		StateVersion: decision2.EpisodeStateVersion,
		StateBefore:  decision2.EpisodeStateBefore,
		Pool:         "openrouter",
		RoutedModel:  decision2.Model,
		Status:       200,
		FinishReason: "length",
		BudgetAction: decision2.BudgetAction,
		TotalTokens:  900,
		Cost:         0.02,
	}
	if err := router.Record(branchRecord); err != nil {
		t.Fatalf("branch Record returned error: %v", err)
	}

	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req3.Header.Set("X-Session-ID", "session-stack")
	body3 := []byte(`{"model":"auto","messages":[{"role":"user","content":"Back to the main task; continue stateful routing."}]}`)
	decision3, err := router.Route(req3, body3)
	if err != nil {
		t.Fatalf("resume Route returned error: %v", err)
	}
	if decision3.EpisodeID != "session-stack" {
		t.Fatalf("resume episode id = %q, want session-stack", decision3.EpisodeID)
	}
	if decision3.EpisodeOperation != "resume" {
		t.Fatalf("resume episode operation = %q, want resume", decision3.EpisodeOperation)
	}
	if decision3.EpisodeStateVersion != 1 {
		t.Fatalf("resume state version = %d, want main state version 1", decision3.EpisodeStateVersion)
	}

	if len(prompts) != 3 {
		t.Fatalf("decision prompts = %d, want 3", len(prompts))
	}
	if !strings.Contains(prompts[1], "episode_id=session-stack#episode-1 state_version=0") {
		t.Fatalf("interrupt prompt missing isolated branch state:\n%s", prompts[1])
	}
	if !strings.Contains(prompts[2], "episode_id=session-stack state_version=1") {
		t.Fatalf("resume prompt missing restored main state:\n%s", prompts[2])
	}
}

func TestClearDecisionStateResetsMainEpisodeStack(t *testing.T) {
	router := newTestSmartRouter("")
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true}

	main := router.resolveSessionEpisode("session-clear", episodeOperationContinue)
	branch := router.resolveSessionEpisode("session-clear", episodeOperationInterrupt)
	if main.EpisodeID != "session-clear" || branch.EpisodeID != "session-clear#episode-1" {
		t.Fatalf("unexpected setup: main=%q branch=%q", main.EpisodeID, branch.EpisodeID)
	}

	router.clearDecisionStateByKey("session-clear")

	router.sessionMu.Lock()
	session := router.sessions["session-clear"]
	router.sessionMu.Unlock()
	if session == nil {
		t.Fatal("session missing after clear")
	}
	if session.ActiveEpisodeID != "session-clear" {
		t.Fatalf("active episode = %q, want session-clear", session.ActiveEpisodeID)
	}
	if len(session.Stack) != 1 || session.Stack[0] != "session-clear" {
		t.Fatalf("session stack = %#v, want only session-clear", session.Stack)
	}
	if session.NextEpisode != 0 {
		t.Fatalf("next episode = %d, want reset to 0", session.NextEpisode)
	}
}

func TestEpisodeOutcomeDoesNotTreatStopAsTaskCompletion(t *testing.T) {
	tests := []struct {
		name   string
		record plugin.AuditRecord
		want   string
	}{
		{
			name: "stop is response completed",
			record: plugin.AuditRecord{
				Status:       200,
				FinishReason: "stop",
				TotalTokens:  100,
			},
			want: "response_completed",
		},
		{
			name: "length is truncation",
			record: plugin.AuditRecord{
				Status:       200,
				FinishReason: "length",
				TotalTokens:  100,
			},
			want: "length_truncated",
		},
		{
			name: "missing finish reason and usage is provider incomplete",
			record: plugin.AuditRecord{
				Status: 200,
			},
			want: "provider_incomplete",
		},
		{
			name: "error kind wins",
			record: plugin.AuditRecord{
				Status:       200,
				FinishReason: "stop",
				TotalTokens:  100,
				ErrorKind:    "proxy_error",
			},
			want: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := episodeOutcome(&tt.record); got != tt.want {
				t.Fatalf("episodeOutcome() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRouteFeedsRecentDecisionHistoryIntoNextPrompt(t *testing.T) {
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode decision request: %v", err)
		}
		if len(payload.Messages) == 0 {
			t.Fatal("decision request had no messages")
		}
		prompts = append(prompts, payload.Messages[0].Content)

		model := "z-ai/glm-5.3-flash"
		summary := "bounded environment scan"
		if len(prompts) == 2 {
			model = "anthropic/claude-opus-5"
			summary = "failed test requires coverage reasoning"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{
			"choices": [{"message": {"content": "{\"model\":\"%s\",\"turn_type\":\"validation\",\"hypothesis_state\":\"stable\",\"critical_path\":false,\"recoverability\":\"easy\",\"context_summary\":\"%s\",\"reason\":\"bounded check\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`, model, summary))
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.CacheTTLSeconds = -1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "trial-history")
	body1 := []byte(`{"model":"auto","messages":[{"role":"user","content":"inspect environment"}]}`)
	body2 := []byte(`{"model":"auto","messages":[{"role":"user","content":"run focused verifier"}]}`)

	if _, err := router.Route(req, body1); err != nil {
		t.Fatalf("first Route returned error: %v", err)
	}
	if _, err := router.Route(req, body2); err != nil {
		t.Fatalf("second Route returned error: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("decision prompts = %d, want 2", len(prompts))
	}
	if strings.Contains(prompts[0], "Recent router memory for this same trial") {
		t.Fatalf("first prompt unexpectedly had history:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[1], "Recent router memory for this same trial") {
		t.Fatalf("second prompt missing history:\n%s", prompts[1])
	}
	if !strings.Contains(prompts[1], "bounded environment scan") {
		t.Fatalf("second prompt missing first decision summary:\n%s", prompts[1])
	}
}

func TestEpisodeLengthFinishBoostsNextBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCheapExecute: {MaxTokens: 1000, TimeoutMs: 10000},
		},
	}
	router.cfg.EpisodeRuntime = EpisodeConfig{
		Enabled:               true,
		LengthStreakThreshold: 1,
		MaxTokensMultiplier:   3,
		TimeoutMultiplier:     2,
		MaxTokensCeiling:      2500,
		TimeoutMsCeiling:      15000,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "episode-budget")
	if err := router.Record(&plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "episode-budget",
		Pool:         "openrouter",
		RoutedModel:  "z-ai/glm-5.3-flash",
		Status:       200,
		FinishReason: "length",
		BudgetAction: budgetActionCheapExecute,
		TotalTokens:  1000,
		Cost:         0.01,
		LatencyMs:    60000,
	}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Run the existing go test ./... command and report the output."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route skipped; want safe-control decision")
	}
	if decision.MaxTokens != 2000 {
		t.Fatalf("max tokens = %d, want 2000 after length boost", decision.MaxTokens)
	}
	if decision.TimeoutMs != 15000 {
		t.Fatalf("timeout ms = %d, want 15000 after capped length boost", decision.TimeoutMs)
	}
	for _, want := range []string{"episode_adjust=length_boost", "episode_calls=1", "episode_length_streak=1", "episode_recent_length=1"} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want %q", decision.Reason, want)
		}
	}
}

func TestEpisodeProgressEventResetsLengthPressureBeforeNextBudget(t *testing.T) {
	var prompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode decision request: %v", err)
		}
		if len(payload.Messages) == 0 {
			t.Fatal("decision request had no messages")
		}
		prompt = payload.Messages[0].Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"turn_type\":\"validation\",\"hypothesis_state\":\"stable\",\"critical_path\":false,\"recoverability\":\"easy\",\"budget_action\":\"cheap_execute\",\"context_summary\":\"test passed\",\"reason\":\"progress observed\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCheapExecute: {MaxTokens: 1000, TimeoutMs: 10000},
		},
	}
	router.cfg.EpisodeRuntime = EpisodeConfig{
		Enabled:               true,
		LengthStreakThreshold: 1,
		MaxTokensMultiplier:   3,
		TimeoutMultiplier:     2,
		MaxTokensCeiling:      2500,
		TimeoutMsCeiling:      15000,
	}
	if err := router.Record(&plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "episode-progress-reset",
		Pool:         "openrouter",
		RoutedModel:  "z-ai/glm-5.3-flash",
		Status:       200,
		FinishReason: "length",
		BudgetAction: budgetActionCheapExecute,
		TotalTokens:  1000,
		Cost:         0.01,
		LatencyMs:    60000,
	}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
	if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
		EventID:   "event-test-run-progress",
		EpisodeID: "episode-progress-reset",
		Timestamp: time.Now(),
		Kind:      "test_run",
		Source:    "unit-test",
		Observation: map[string]any{
			"outcome": "passed",
			"command": "go test ./...",
		},
		EvidenceRefs: []string{"stdout"},
	}); err != nil {
		t.Fatalf("RecordEpisodeEvent returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "episode-progress-reset")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Summarize the validation result and continue."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route skipped; want routed decision")
	}
	if decision.MaxTokens != 1000 {
		t.Fatalf("max tokens = %d, want base budget after progress reset", decision.MaxTokens)
	}
	if strings.Contains(decision.Reason, "episode_adjust=length_boost") {
		t.Fatalf("reason = %q, want no length boost after progress event", decision.Reason)
	}
	for _, want := range []string{
		"test_runs=1",
		"test_passed=1",
		"candidate=1",
		"length_since_progress=0",
		"last_progress=test_run",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestEpisodeNoProgressEventFreezesLengthBudgetExpansion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCheapExecute:   {MaxTokens: 1000, TimeoutMs: 10000},
			budgetActionPremiumRecover: {MaxTokens: 1000, TimeoutMs: 10000},
		},
	}
	router.cfg.EpisodeRuntime = EpisodeConfig{
		Enabled:               true,
		LengthStreakThreshold: 1,
		MaxTokensMultiplier:   3,
		TimeoutMultiplier:     2,
		MaxTokensCeiling:      2500,
		TimeoutMsCeiling:      15000,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "episode-no-progress")
	if err := router.Record(&plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "episode-no-progress",
		Pool:         "openrouter",
		RoutedModel:  "z-ai/glm-5.3-flash",
		Status:       200,
		FinishReason: "length",
		BudgetAction: budgetActionCheapExecute,
		TotalTokens:  1000,
		Cost:         0.01,
		LatencyMs:    60000,
	}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
	if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
		EventID:   "event-no-progress-1",
		EpisodeID: "episode-no-progress",
		Timestamp: time.Now(),
		Kind:      "no_progress",
		Source:    "progress-reducer",
		Observation: map[string]any{
			"reason":     "length_pressure_without_progress",
			"since_turn": 3,
		},
		EvidenceRefs: []string{"event:length-1"},
	}); err != nil {
		t.Fatalf("RecordEpisodeEvent returned error: %v", err)
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Run the existing go test ./... command and report the output."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route skipped; want safe-control decision")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want Opus recovery route", decision.Model)
	}
	if decision.BudgetAction != budgetActionPremiumRecover {
		t.Fatalf("budget action = %q, want %s", decision.BudgetAction, budgetActionPremiumRecover)
	}
	if decision.MaxTokens != 1000 {
		t.Fatalf("max tokens = %d, want frozen base budget", decision.MaxTokens)
	}
	if decision.TimeoutMs != 10000 {
		t.Fatalf("timeout ms = %d, want frozen base timeout", decision.TimeoutMs)
	}
	for _, want := range []string{"rule_id=episode_no_progress_recovery", "episode_adjust=no_progress_freeze", "episode_no_progress=stale"} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want %q", decision.Reason, want)
		}
	}

	if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
		EventID:   "event-test-progress-after-freeze",
		EpisodeID: "episode-no-progress",
		Timestamp: time.Now(),
		Kind:      "test_run",
		Source:    "unit-test",
		Observation: map[string]any{
			"outcome": "passed",
			"command": "go test ./...",
		},
		EvidenceRefs: []string{"stdout"},
	}); err != nil {
		t.Fatalf("RecordEpisodeEvent progress returned error: %v", err)
	}
	reqAfterProgress := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqAfterProgress.Header.Set("X-Session-ID", "episode-no-progress")
	decisionAfterProgress, err := router.Route(reqAfterProgress, body)
	if err != nil {
		t.Fatalf("Route after progress returned error: %v", err)
	}
	if strings.Contains(decisionAfterProgress.Reason, "episode_adjust=no_progress_freeze") {
		t.Fatalf("reason = %q, want progress event to clear active no-progress freeze", decisionAfterProgress.Reason)
	}
}

func TestRecordEpisodeEventDedupesEventIDsBeforeProjection(t *testing.T) {
	router := newTestSmartRouter("")
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true}
	event := &plugin.EpisodeEvent{
		EventID:   "event-dedupe-test-run",
		EpisodeID: "episode-dedupe",
		Timestamp: time.Now(),
		Kind:      "test_run",
		Source:    "unit-test",
		Observation: map[string]any{
			"outcome": "passed",
			"command": "go test ./...",
		},
	}

	if err := router.RecordEpisodeEvent(event); err != nil {
		t.Fatalf("first RecordEpisodeEvent returned error: %v", err)
	}
	if err := router.RecordEpisodeEvent(event); err != nil {
		t.Fatalf("second RecordEpisodeEvent returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Episode-ID", "episode-dedupe")
	snapshot := router.episodeSnapshot(req)
	if snapshot.Version != 1 {
		t.Fatalf("state version = %d, want 1 after duplicate event id", snapshot.Version)
	}
	if snapshot.TestRunCount != 1 || snapshot.TestPassedCount != 1 {
		t.Fatalf("test counts = run %d passed %d, want 1/1", snapshot.TestRunCount, snapshot.TestPassedCount)
	}
	if snapshot.CandidateProgressCount != 1 {
		t.Fatalf("candidate progress = %d, want 1", snapshot.CandidateProgressCount)
	}
	if len(snapshot.RecentEvents) != 1 {
		t.Fatalf("recent events = %d, want 1", len(snapshot.RecentEvents))
	}
}

func TestQueryEpisodeStatesReturnsCurrentProjection(t *testing.T) {
	router := newTestSmartRouter("")
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true}
	if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
		EventID:   "event-query-test-run",
		EpisodeID: "episode-query",
		Timestamp: time.Now(),
		Kind:      "test_run",
		Source:    "unit-test",
		Observation: map[string]any{
			"outcome": "passed",
			"command": "go test ./...",
		},
	}); err != nil {
		t.Fatalf("RecordEpisodeEvent returned error: %v", err)
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-query"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	state := states[0]
	if state.EpisodeID != "episode-query" || state.Source != "smart-router" {
		t.Fatalf("state identity = %#v, want episode-query from smart-router", state)
	}
	if state.StateVersion != 1 {
		t.Fatalf("state version = %d, want 1", state.StateVersion)
	}
	if got := state.State["test_run_count"]; got != 1 {
		t.Fatalf("test_run_count = %#v, want 1", got)
	}
	if got := state.State["last_progress_kind"]; got != "test_run" {
		t.Fatalf("last_progress_kind = %#v, want test_run", got)
	}

	allStates, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{})
	if err != nil {
		t.Fatalf("QueryEpisodeStates all returned error: %v", err)
	}
	if len(allStates) != 1 || allStates[0].EpisodeID != "episode-query" {
		t.Fatalf("all states = %#v, want episode-query", allStates)
	}
}

func TestEpisodeStateBackfillInfluencesNextRoute(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionPremiumRecover: {MaxTokens: 1000, TimeoutMs: 10000},
		},
	}
	router.cfg.EpisodeRuntime = EpisodeConfig{
		Enabled:               true,
		RecentEvents:          5,
		LengthStreakThreshold: 3,
		LengthWindowThreshold: 2,
		MaxTokensMultiplier:   2,
		TimeoutMultiplier:     2,
		MaxTokensCeiling:      3000,
		TimeoutMsCeiling:      20000,
	}
	router.SetStateBackfillSources([]plugin.TraceQueryer{fakeTraceQueryer{
		traces: []plugin.TraceEntry{
			{
				TraceID:      "trace-backfill-1",
				Timestamp:    "2026-09-10T10:00:00Z",
				RoutedModel:  "z-ai/glm-5.3-flash",
				Pool:         "openrouter",
				SessionID:    "episode-backfill",
				EpisodeID:    "episode-backfill",
				Status:       200,
				FinishReason: "length",
				BudgetAction: budgetActionCheapExecute,
				TotalTokens:  1000,
				Cost:         0.01,
				LatencyMs:    60000,
			},
			{
				TraceID:      "trace-backfill-2",
				Timestamp:    "2026-09-10T10:01:00Z",
				RoutedModel:  "z-ai/glm-5.3-flash",
				Pool:         "openrouter",
				SessionID:    "episode-backfill",
				EpisodeID:    "episode-backfill",
				Status:       200,
				FinishReason: "length",
				BudgetAction: budgetActionCheapExecute,
				TotalTokens:  1000,
				Cost:         0.01,
				LatencyMs:    60000,
			},
		},
	}}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Episode-ID", "episode-backfill")
	req.Header.Set("X-Session-ID", "episode-backfill")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue with the next bounded step."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want local route after state backfill")
	}
	if decision == nil || decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("decision = %#v, want Opus recovery", decision)
	}
	if !strings.Contains(decision.Reason, "rule_id=episode_no_progress_recovery") {
		t.Fatalf("reason = %q, want episode recovery rule", decision.Reason)
	}
	if decision.EpisodeStateVersion != 2 {
		t.Fatalf("state version before route = %d, want 2 from backfilled traces", decision.EpisodeStateVersion)
	}
	if !strings.Contains(decision.EpisodeStateBefore, `"no_progress_severity":"stale"`) {
		t.Fatalf("state before = %q, want stale no-progress", decision.EpisodeStateBefore)
	}
}

func TestQueryEpisodeStatesBackfillsPersistedEvents(t *testing.T) {
	router := newTestSmartRouter("")
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true}
	router.SetStateBackfillSources(nil, []plugin.EpisodeEventQueryer{fakeEpisodeEventQueryer{
		events: []plugin.EpisodeEvent{
			{
				EventID:   "event-backfilled-test",
				EpisodeID: "episode-events-backfill",
				Timestamp: time.Now(),
				Kind:      "test_run",
				Source:    "audit-store",
				Observation: map[string]any{
					"outcome": "passed",
					"command": "go test ./...",
				},
			},
		},
	}})

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-events-backfill"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if states[0].StateVersion != 1 {
		t.Fatalf("state version = %d, want 1", states[0].StateVersion)
	}
	if got := states[0].State["test_passed_count"]; got != 1 {
		t.Fatalf("test_passed_count = %#v, want 1", got)
	}
}

func TestEpisodeRepeatedTestFailureRoutesPremiumRecoveryOnce(t *testing.T) {
	decisionCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"turn_type\":\"implementation\",\"hypothesis_state\":\"forming\",\"critical_path\":false,\"recoverability\":\"medium\",\"budget_action\":\"cheap_probe\",\"context_summary\":\"reassess after local recovery\",\"reason\":\"semantic judge resumes after one local repeated-failure recovery\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCheapProbe:     {MaxTokens: 500, TimeoutMs: 5000},
			budgetActionPremiumRecover: {MaxTokens: 1000, TimeoutMs: 10000},
		},
	}
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}
	for i := 0; i < 2; i++ {
		if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
			EventID:   fmt.Sprintf("event-repeated-failure-%d", i+1),
			EpisodeID: "episode-repeated-failure",
			Timestamp: time.Now(),
			Kind:      "test_run",
			Source:    "unit-test",
			Observation: map[string]any{
				"outcome":             "failed",
				"command":             "go test ./...",
				"failure_fingerprint": "AssertionError: expected relay id 7 got 8",
				"failed_count":        2,
			},
			EvidenceRefs: []string{"stdout"},
		}); err != nil {
			t.Fatalf("RecordEpisodeEvent %d returned error: %v", i+1, err)
		}
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-repeated-failure"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if got := states[0].State["same_failure_fingerprint_count"]; got != 2 {
		t.Fatalf("same failure count = %#v, want 2", got)
	}
	if got := states[0].State["failure_frontier_size"]; got != 2 {
		t.Fatalf("failure frontier size = %#v, want 2", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Episode-ID", "episode-repeated-failure")
	req.Header.Set("X-Session-ID", "episode-repeated-failure")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue with the next bounded implementation step."}]}`)
	first, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("first Route returned error: %v", err)
	}
	if decisionCalls != 0 {
		t.Fatal("decision server was called; want first repeated failure handled locally")
	}
	if first == nil || first.Model != "anthropic/claude-opus-5" {
		t.Fatalf("first decision = %#v, want Opus recovery", first)
	}
	if first.BudgetAction != budgetActionPremiumRecover {
		t.Fatalf("first budget action = %q, want %s", first.BudgetAction, budgetActionPremiumRecover)
	}
	for _, want := range []string{
		"rule_id=episode_repeated_failure_recovery",
		"same_failure_count=2",
		"failure_frontier_size=2",
		"failure_fingerprint=assertionerror: expected relay id # got #",
	} {
		if !strings.Contains(first.Reason, want) {
			t.Fatalf("first reason = %q, want %q", first.Reason, want)
		}
	}

	second, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("second Route returned error: %v", err)
	}
	if decisionCalls != 1 {
		t.Fatalf("decision server calls = %d, want semantic judge after one local recovery", decisionCalls)
	}
	if second == nil || second.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("second decision = %#v, want decision-model route", second)
	}
	if strings.Contains(second.Reason, "rule_id=episode_repeated_failure_recovery") {
		t.Fatalf("second reason = %q, want no repeated local recovery for same fingerprint", second.Reason)
	}
}

func TestEpisodeFailureFrontierReductionDoesNotTriggerRepeatedRecovery(t *testing.T) {
	decisionCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"turn_type\":\"validation\",\"hypothesis_state\":\"stable\",\"critical_path\":false,\"recoverability\":\"easy\",\"budget_action\":\"cheap_probe\",\"context_summary\":\"frontier shrank\",\"reason\":\"continue cheap after observed failure reduction\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}
	for i, failedCount := range []int{3, 1} {
		if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
			EventID:   fmt.Sprintf("event-frontier-reduced-%d", i+1),
			EpisodeID: "episode-frontier-reduced",
			Timestamp: time.Now(),
			Kind:      "test_run",
			Source:    "unit-test",
			Observation: map[string]any{
				"outcome":             "failed",
				"command":             "go test ./...",
				"failure_fingerprint": "AssertionError: expected relay id 7 got 8",
				"failed_count":        failedCount,
			},
		}); err != nil {
			t.Fatalf("RecordEpisodeEvent %d returned error: %v", i+1, err)
		}
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-frontier-reduced"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if got := states[0].State["same_failure_fingerprint_count"]; got != 1 {
		t.Fatalf("same failure count = %#v, want reset after frontier shrink", got)
	}
	if got := states[0].State["failure_frontier_size"]; got != 1 {
		t.Fatalf("failure frontier size = %#v, want latest reduced size", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Episode-ID", "episode-frontier-reduced")
	req.Header.Set("X-Session-ID", "episode-frontier-reduced")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue with the next bounded implementation step."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionCalls != 1 {
		t.Fatalf("decision server calls = %d, want semantic judge because frontier shrank", decisionCalls)
	}
	if decision == nil || decision.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("decision = %#v, want decision-model route", decision)
	}
	if strings.Contains(decision.Reason, "rule_id=episode_repeated_failure_recovery") {
		t.Fatalf("reason = %q, want no local repeated-failure recovery after frontier shrink", decision.Reason)
	}
}

func TestEpisodePassingTestClearsFailureFrontier(t *testing.T) {
	router := newTestSmartRouter("")
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true, RecentEvents: 5}
	if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
		EventID:   "event-frontier-failed",
		EpisodeID: "episode-frontier-clear",
		Timestamp: time.Now(),
		Kind:      "test_failed",
		Source:    "unit-test",
		Observation: map[string]any{
			"failed_count":         2,
			"failure_fingerprints": []string{"TestAlpha", "TestBeta"},
		},
	}); err != nil {
		t.Fatalf("RecordEpisodeEvent failed returned error: %v", err)
	}
	if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
		EventID:   "event-frontier-passed",
		EpisodeID: "episode-frontier-clear",
		Timestamp: time.Now(),
		Kind:      "test_run",
		Source:    "unit-test",
		Observation: map[string]any{
			"outcome":      "passed",
			"passed_count": 8,
			"failed_count": 0,
		},
	}); err != nil {
		t.Fatalf("RecordEpisodeEvent passed returned error: %v", err)
	}

	states, err := router.QueryEpisodeStates(plugin.EpisodeStateFilter{EpisodeID: "episode-frontier-clear"})
	if err != nil {
		t.Fatalf("QueryEpisodeStates returned error: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if got := states[0].State["failure_frontier_size"]; got != 0 {
		t.Fatalf("failure frontier size = %#v, want cleared", got)
	}
	if got := states[0].State["same_failure_fingerprint_count"]; got != 0 {
		t.Fatalf("same failure count = %#v, want cleared", got)
	}
	if got := states[0].State["last_failure_fingerprint"]; got != "" {
		t.Fatalf("last failure fingerprint = %#v, want cleared", got)
	}
}

func TestEpisodeNoProgressStateRoutesPremiumRecoveryWithoutDecisionModel(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionPremiumRecover: {MaxTokens: 1000, TimeoutMs: 10000},
		},
	}
	router.cfg.EpisodeRuntime = EpisodeConfig{
		Enabled:               true,
		RecentEvents:          5,
		LengthStreakThreshold: 3,
		LengthWindowThreshold: 2,
		MaxTokensMultiplier:   2,
		TimeoutMultiplier:     2,
		MaxTokensCeiling:      3000,
		TimeoutMsCeiling:      20000,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "episode-state-recovery")
	for i := 0; i < 2; i++ {
		if err := router.Record(&plugin.AuditRecord{
			Timestamp:    time.Now(),
			SessionID:    "episode-state-recovery",
			Pool:         "openrouter",
			RoutedModel:  "z-ai/glm-5.3-flash",
			Status:       200,
			FinishReason: "length",
			BudgetAction: budgetActionCheapExecute,
			TotalTokens:  1000,
			Cost:         0.01,
			LatencyMs:    60000,
		}); err != nil {
			t.Fatalf("Record %d returned error: %v", i+1, err)
		}
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Run the existing go test ./... command and report the output."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want local episode state recovery")
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want episode state recovery route")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want Opus", decision.Model)
	}
	if decision.BudgetAction != budgetActionPremiumRecover {
		t.Fatalf("budget action = %q, want %s", decision.BudgetAction, budgetActionPremiumRecover)
	}
	if decision.MaxTokens != 1000 {
		t.Fatalf("max tokens = %d, want frozen base budget", decision.MaxTokens)
	}
	if decision.TimeoutMs != 10000 {
		t.Fatalf("timeout ms = %d, want frozen base timeout", decision.TimeoutMs)
	}
	for _, want := range []string{
		"rule_id=episode_no_progress_recovery",
		"no_progress=stale",
		"state_version=2",
		"recent_length=2",
		"llm_since_progress=2",
		"episode_adjust=no_progress_freeze",
	} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want %q", decision.Reason, want)
		}
	}
}

func TestEpisodeNoProgressRecoveryHonorsPremiumCooldown(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.EpisodeRuntime = EpisodeConfig{Enabled: true}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "episode-state-cooldown")
	if err := router.RecordEpisodeEvent(&plugin.EpisodeEvent{
		EventID:   "event-no-progress-cooldown",
		EpisodeID: "episode-state-cooldown",
		Timestamp: time.Now(),
		Kind:      "no_progress",
		Source:    "progress-reducer",
		Observation: map[string]any{
			"reason": "length_pressure_without_progress",
		},
	}); err != nil {
		t.Fatalf("RecordEpisodeEvent returned error: %v", err)
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue with the next bounded step."}]}`)
	for i := 0; i < 2; i++ {
		decision, err := router.Route(req, body)
		if err != nil {
			t.Fatalf("recovery Route %d returned error: %v", i+1, err)
		}
		if decision == nil || decision.Model != "anthropic/claude-opus-5" {
			t.Fatalf("recovery Route %d = %#v, want Opus recovery", i+1, decision)
		}
		if !strings.Contains(decision.Reason, "rule_id=episode_no_progress_recovery") {
			t.Fatalf("recovery Route %d reason = %q, want episode recovery rule", i+1, decision.Reason)
		}
	}

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("cooldown Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want local cooldown route")
	}
	if decision == nil || decision.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("cooldown Route = %#v, want flash", decision)
	}
	if !strings.Contains(decision.Reason, "rule_id=premium_cooldown") {
		t.Fatalf("cooldown reason = %q, want premium cooldown rule", decision.Reason)
	}
	if strings.Contains(decision.Reason, "rule_id=episode_no_progress_recovery") {
		t.Fatalf("cooldown reason = %q, no-progress recovery should honor cooldown", decision.Reason)
	}
}

func TestEpisodeRecentLengthPressureFreezesBudgetWhenStale(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionPremiumRecover: {MaxTokens: 1000, TimeoutMs: 10000},
		},
	}
	router.cfg.EpisodeRuntime = EpisodeConfig{
		Enabled:               true,
		RecentEvents:          5,
		LengthStreakThreshold: 3,
		LengthWindowThreshold: 2,
		MaxTokensMultiplier:   3,
		TimeoutMultiplier:     2,
		MaxTokensCeiling:      2500,
		TimeoutMsCeiling:      15000,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "episode-recent-pressure")
	for _, finish := range []string{"length", "stop", "length"} {
		if err := router.Record(&plugin.AuditRecord{
			Timestamp:    time.Now(),
			SessionID:    "episode-recent-pressure",
			Pool:         "openrouter",
			RoutedModel:  "z-ai/glm-5.3-flash",
			Status:       200,
			FinishReason: finish,
			BudgetAction: budgetActionCheapExecute,
			TotalTokens:  1000,
			Cost:         0.01,
			LatencyMs:    60000,
		}); err != nil {
			t.Fatalf("Record returned error: %v", err)
		}
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"The hypothesis is contradicted by the latest test output; recover the approach."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route skipped; want safe-control premium recovery decision")
	}
	if decision.BudgetAction != budgetActionPremiumRecover {
		t.Fatalf("budget action = %q, want %s", decision.BudgetAction, budgetActionPremiumRecover)
	}
	if decision.MaxTokens != 1000 {
		t.Fatalf("max tokens = %d, want frozen base budget", decision.MaxTokens)
	}
	if decision.TimeoutMs != 10000 {
		t.Fatalf("timeout ms = %d, want frozen base timeout", decision.TimeoutMs)
	}
	for _, want := range []string{"episode_adjust=no_progress_freeze", "episode_no_progress=stale", "episode_calls=3", "episode_length_streak=1", "episode_recent_length=2"} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("reason = %q, want %q", decision.Reason, want)
		}
	}
}

func TestSmartRouterAppliesBudgetActionFromDecisionModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"turn_type\":\"validation\",\"hypothesis_state\":\"stable\",\"critical_path\":false,\"recoverability\":\"easy\",\"budget_action\":\"cheap_execute\",\"context_summary\":\"known test command\",\"reason\":\"bounded validation\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	router.cfg.CacheTTLSeconds = -1
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCheapExecute: {MaxTokens: 777, TimeoutMs: 888},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Run the known test command."}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route skipped; want budgeted decision")
	}
	if decision.BudgetAction != budgetActionCheapExecute {
		t.Fatalf("budget action = %q, want %s", decision.BudgetAction, budgetActionCheapExecute)
	}
	if decision.MaxTokens != 777 {
		t.Fatalf("max tokens = %d, want 777", decision.MaxTokens)
	}
	if decision.TimeoutMs != 888 {
		t.Fatalf("timeout ms = %d, want 888", decision.TimeoutMs)
	}
	if !strings.Contains(decision.Reason, "budget_action=cheap_execute") {
		t.Fatalf("reason = %q, want budget action marker", decision.Reason)
	}
}

func TestSafeControlRoutesCheapHighConfidenceRequestsWithoutDecisionModel(t *testing.T) {
	tests := []struct {
		name       string
		message    string
		wantRuleID string
	}{
		{
			name:       "file read and search",
			message:    "Use rg to search for the handler and inspect the matching files.",
			wantRuleID: "file_read_search_cheap",
		},
		{
			name:       "existing test execution",
			message:    "Run the existing go test ./... command and report the output.",
			wantRuleID: "existing_test_execution_cheap",
		},
		{
			name:       "fixed format output",
			message:    "Respond with JSON only: {\"cmd\":\"pwd\"}. Do not include any text outside the JSON.",
			wantRuleID: "fixed_format_output_cheap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decisionServerCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				decisionServerCalled = true
				http.Error(w, "decision model should not be called", http.StatusInternalServerError)
			}))
			defer server.Close()

			router := newTestSmartRouter(server.URL)
			enableSafeControl(router)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("X-Session-ID", "safe-cheap-"+tt.name)
			body := []byte(fmt.Sprintf(`{"model":"auto","messages":[{"role":"user","content":%q}]}`, tt.message))

			decision, err := router.Route(req, body)
			if err != nil {
				t.Fatalf("Route returned error: %v", err)
			}
			if decisionServerCalled {
				t.Fatal("decision server was called; want local safe-control decision")
			}
			if decision == nil || decision.Skip {
				t.Fatalf("Route skipped; want safe-control cheap route")
			}
			if decision.Model != "z-ai/glm-5.3-flash" {
				t.Fatalf("model = %q, want z-ai/glm-5.3-flash", decision.Model)
			}
			for _, want := range []string{"decision_source=rule", "action=cheap", "rule_id=" + tt.wantRuleID} {
				if !strings.Contains(decision.Reason, want) {
					t.Fatalf("reason = %q, want %q", decision.Reason, want)
				}
			}
		})
	}
}

func TestSafeControlAppliesBudgetProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.BudgetedRoute = BudgetedRouteConfig{
		Enabled: true,
		Profiles: map[string]RouteBudgetProfile{
			budgetActionCheapProbe: {MaxTokens: 321, TimeoutMs: 654},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-budget-profile")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Use rg to search for the handler and inspect the matching files."}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatal("Route skipped; want safe-control decision")
	}
	if decision.BudgetAction != budgetActionCheapProbe {
		t.Fatalf("budget action = %q, want %s", decision.BudgetAction, budgetActionCheapProbe)
	}
	if decision.MaxTokens != 321 {
		t.Fatalf("max tokens = %d, want 321", decision.MaxTokens)
	}
	if decision.TimeoutMs != 654 {
		t.Fatalf("timeout ms = %d, want 654", decision.TimeoutMs)
	}
	if !strings.Contains(decision.Reason, "route_max_tokens=321") {
		t.Fatalf("reason = %q, want route max tokens marker", decision.Reason)
	}
}

func TestSafeControlCheapProbeBurstFallsThroughToDecisionModel(t *testing.T) {
	decisionServerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"anthropic/claude-opus-5\",\"turn_type\":\"planning\",\"hypothesis_state\":\"forming\",\"critical_path\":true,\"recoverability\":\"hard\",\"context_summary\":\"cheap probes saturated\",\"reason\":\"re-evaluate after repeated cheap probes\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.SafeControl.CheapProbeBurstLimit = 2
	router.cfg.CacheTTLSeconds = -1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-cheap-burst")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Use rg to search for the handler and inspect the matching files."}]}`)

	for i := 0; i < 2; i++ {
		decision, err := router.Route(req, body)
		if err != nil {
			t.Fatalf("cheap Route %d returned error: %v", i+1, err)
		}
		if decision == nil || decision.Model != "z-ai/glm-5.3-flash" {
			t.Fatalf("cheap Route %d = %#v, want flash", i+1, decision)
		}
		if !strings.Contains(decision.Reason, "rule_id=file_read_search_cheap") {
			t.Fatalf("cheap Route %d reason = %q, want safe-control file/search rule", i+1, decision.Reason)
		}
	}

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("fallthrough Route returned error: %v", err)
	}
	if decision == nil || decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("fallthrough Route = %#v, want Opus from decision model", decision)
	}
	if strings.Contains(decision.Reason, "safe-control") {
		t.Fatalf("fallthrough reason = %q, want semantic smart-router route", decision.Reason)
	}
	if decisionServerCalls != 1 {
		t.Fatalf("decision server calls = %d, want 1", decisionServerCalls)
	}
}

func TestSafeControlRepeatedErrorUpgradesWithoutSecondDecisionModelCall(t *testing.T) {
	decisionServerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"turn_type\":\"validation\",\"hypothesis_state\":\"stable\",\"critical_path\":false,\"recoverability\":\"easy\",\"context_summary\":\"first failure observed\",\"reason\":\"collect evidence\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.CacheTTLSeconds = -1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-repeated-error")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"pytest failed\nAssertionError: expected 1 got 2"}]}`)

	first, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("first Route returned error: %v", err)
	}
	if first == nil || first.Skip {
		t.Fatalf("first Route skipped; want decision-model route")
	}
	if first.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("first model = %q, want flash from decision model", first.Model)
	}

	second, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("second Route returned error: %v", err)
	}
	if second == nil || second.Skip {
		t.Fatalf("second Route skipped; want repeated-error upgrade")
	}
	if second.Model != "anthropic/claude-opus-5" {
		t.Fatalf("second model = %q, want Opus", second.Model)
	}
	if !strings.Contains(second.Reason, "rule_id=repeated_error_upgrade") {
		t.Fatalf("second reason = %q, want repeated error rule", second.Reason)
	}
	if decisionServerCalls != 1 {
		t.Fatalf("decision server calls = %d, want 1", decisionServerCalls)
	}
}

func TestSafeControlDoesNotTreatProviderFailureAsRepeatedModelError(t *testing.T) {
	decisionServerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"reason\":\"provider issue is infrastructure\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	router.cfg.CacheTTLSeconds = -1
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-provider-failure")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"OpenRouter 502 Bad Gateway\nprovider timeout while calling upstream"}]}`)

	for i := 0; i < 2; i++ {
		decision, err := router.Route(req, body)
		if err != nil {
			t.Fatalf("Route %d returned error: %v", i+1, err)
		}
		if decision == nil || decision.Skip {
			t.Fatalf("Route %d skipped; want decision-model route", i+1)
		}
		if decision.Model != "z-ai/glm-5.3-flash" {
			t.Fatalf("Route %d model = %q, want flash from decision model", i+1, decision.Model)
		}
		if strings.Contains(decision.Reason, "repeated_error_upgrade") {
			t.Fatalf("Route %d reason = %q, provider failure must not become model-ability repeated error", i+1, decision.Reason)
		}
	}
	if decisionServerCalls != 2 {
		t.Fatalf("decision server calls = %d, want 2", decisionServerCalls)
	}
}

func TestSafeControlHypothesisContradictionUpgradesWithoutDecisionModel(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-contradiction")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"The core hypothesis was contradicted by a counterexample; recover before editing more files."}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want local contradiction upgrade")
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want premium route")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want Opus", decision.Model)
	}
	if !strings.Contains(decision.Reason, "rule_id=hypothesis_contradiction_upgrade") {
		t.Fatalf("reason = %q, want contradiction rule", decision.Reason)
	}
}

func TestSafeControlPremiumCooldownRoutesAmbiguousCallCheap(t *testing.T) {
	decisionServerCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalled = true
		http.Error(w, "decision model should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-cooldown")
	premiumBody := []byte(`{"model":"auto","messages":[{"role":"user","content":"The current hypothesis was contradicted; use recovery reasoning."}]}`)

	for i := 0; i < 2; i++ {
		decision, err := router.Route(req, premiumBody)
		if err != nil {
			t.Fatalf("premium Route %d returned error: %v", i+1, err)
		}
		if decision == nil || decision.Model != "anthropic/claude-opus-5" {
			t.Fatalf("premium Route %d = %#v, want Opus", i+1, decision)
		}
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue with the next bounded step."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("cooldown Route returned error: %v", err)
	}
	if decisionServerCalled {
		t.Fatal("decision server was called; want local cooldown route")
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want cooldown cheap route")
	}
	if decision.Model != "z-ai/glm-5.3-flash" {
		t.Fatalf("model = %q, want flash", decision.Model)
	}
	if !strings.Contains(decision.Reason, "rule_id=premium_cooldown") {
		t.Fatalf("reason = %q, want cooldown rule", decision.Reason)
	}
}

func TestTaskCompletionConfirmationClearsSafeControlState(t *testing.T) {
	decisionServerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"z-ai/glm-5.3-flash\",\"reason\":\"fresh task after completion\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-completion-reset")
	premiumBody := []byte(`{"model":"auto","messages":[{"role":"user","content":"The current hypothesis was contradicted; use recovery reasoning."}]}`)

	for i := 0; i < 2; i++ {
		decision, err := router.Route(req, premiumBody)
		if err != nil {
			t.Fatalf("premium Route %d returned error: %v", i+1, err)
		}
		if decision == nil || decision.Model != "anthropic/claude-opus-5" {
			t.Fatalf("premium Route %d = %#v, want Opus", i+1, decision)
		}
	}

	completionBody := []byte(`{
		"model": "auto",
		"messages": [
			{"role": "user", "content": "Are you sure you want to mark the task as complete? Include \"task_complete\": true."}
		]
	}`)
	var completionDecision *plugin.RoutingDecision
	if decision, err := router.Route(req, completionBody); err != nil {
		t.Fatalf("completion Route returned error: %v", err)
	} else if decision == nil || decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("completion Route = %#v, want Opus", decision)
	} else {
		completionDecision = decision
	}
	if err := router.Record(&plugin.AuditRecord{
		Timestamp:    time.Now(),
		SessionID:    "safe-completion-reset",
		Pool:         "openrouter",
		RoutedModel:  completionDecision.Model,
		Status:       200,
		FinishReason: "stop",
		BudgetAction: completionDecision.BudgetAction,
	}); err != nil {
		t.Fatalf("completion Record returned error: %v", err)
	}

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Continue with the next bounded step."}]}`)
	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("post-completion Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("post-completion Route skipped; want decision-model route")
	}
	if decisionServerCalls != 1 {
		t.Fatalf("decision server calls = %d, want 1 after completion reset", decisionServerCalls)
	}
	if strings.Contains(decision.Reason, "premium_cooldown") {
		t.Fatalf("reason = %q, stale cooldown state leaked after completion", decision.Reason)
	}
}

func TestSafeControlFallsThroughToDecisionModelForAmbiguousRequests(t *testing.T) {
	decisionServerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionServerCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices": [{"message": {"content": "{\"model\":\"anthropic/claude-opus-5\",\"reason\":\"semantic judge decides\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	router := newTestSmartRouter(server.URL)
	enableSafeControl(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "safe-fallthrough")
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Design the state transition model for task episodes."}]}`)

	decision, err := router.Route(req, body)
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if decision == nil || decision.Skip {
		t.Fatalf("Route skipped; want decision-model route")
	}
	if decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("model = %q, want Opus from decision model", decision.Model)
	}
	if decisionServerCalls != 1 {
		t.Fatalf("decision server calls = %d, want 1", decisionServerCalls)
	}
	if strings.Contains(decision.Reason, "safe-control") {
		t.Fatalf("reason = %q, want semantic smart-router route", decision.Reason)
	}
}

func TestConfigParsesSafeControl(t *testing.T) {
	cfg, err := config.LoadFromBytes([]byte(`
routes:
  - pattern: /v1/chat/completions
    pool: openrouter
pools:
  openrouter:
    endpoints:
      - name: fake
        url: http://127.0.0.1:1
plugins:
  smart-router:
    enabled: true
    endpoint: http://127.0.0.1:2/v1
    model: decision-model
    safe_control:
      enabled: true
      repeated_error_threshold: 3
      premium_cooldown_after: 4
      premium_cooldown_turns: 2
      cheap_probe_burst_limit: 5
    budgeted_route:
      enabled: true
      profiles:
        cheap_probe:
          max_tokens: 111
          timeout_ms: 222
    episode_runtime:
      enabled: true
      recent_events: 7
      length_streak_threshold: 2
      length_window_threshold: 3
      max_tokens_multiplier: 4
      timeout_multiplier: 2
      max_tokens_ceiling: 9000
      timeout_ms_ceiling: 180000
`))
	if err != nil {
		t.Fatalf("LoadFromBytes returned error: %v", err)
	}
	smartCfg, ok := config.PluginConfig[Config](cfg, "smart-router")
	if !ok {
		t.Fatal("smart-router config missing")
	}
	if !smartCfg.SafeControl.Enabled {
		t.Fatal("safe_control.enabled = false, want true")
	}
	if smartCfg.SafeControl.RepeatedErrorThreshold != 3 {
		t.Fatalf("repeated_error_threshold = %d, want 3", smartCfg.SafeControl.RepeatedErrorThreshold)
	}
	if smartCfg.SafeControl.PremiumCooldownAfter != 4 {
		t.Fatalf("premium_cooldown_after = %d, want 4", smartCfg.SafeControl.PremiumCooldownAfter)
	}
	if smartCfg.SafeControl.PremiumCooldownTurns != 2 {
		t.Fatalf("premium_cooldown_turns = %d, want 2", smartCfg.SafeControl.PremiumCooldownTurns)
	}
	if smartCfg.SafeControl.CheapProbeBurstLimit != 5 {
		t.Fatalf("cheap_probe_burst_limit = %d, want 5", smartCfg.SafeControl.CheapProbeBurstLimit)
	}
	if !smartCfg.BudgetedRoute.Enabled {
		t.Fatal("budgeted_route.enabled = false, want true")
	}
	if got := smartCfg.BudgetedRoute.Profiles[budgetActionCheapProbe].MaxTokens; got != 111 {
		t.Fatalf("cheap_probe max_tokens = %d, want 111", got)
	}
	if got := smartCfg.BudgetedRoute.Profiles[budgetActionCheapProbe].TimeoutMs; got != 222 {
		t.Fatalf("cheap_probe timeout_ms = %d, want 222", got)
	}
	if !smartCfg.EpisodeRuntime.Enabled {
		t.Fatal("episode_runtime.enabled = false, want true")
	}
	if smartCfg.EpisodeRuntime.RecentEvents != 7 {
		t.Fatalf("episode recent events = %d, want 7", smartCfg.EpisodeRuntime.RecentEvents)
	}
	if smartCfg.EpisodeRuntime.LengthStreakThreshold != 2 {
		t.Fatalf("episode length threshold = %d, want 2", smartCfg.EpisodeRuntime.LengthStreakThreshold)
	}
	if smartCfg.EpisodeRuntime.LengthWindowThreshold != 3 {
		t.Fatalf("episode length window threshold = %d, want 3", smartCfg.EpisodeRuntime.LengthWindowThreshold)
	}
	if smartCfg.EpisodeRuntime.MaxTokensMultiplier != 4 {
		t.Fatalf("episode max token multiplier = %f, want 4", smartCfg.EpisodeRuntime.MaxTokensMultiplier)
	}
	if smartCfg.EpisodeRuntime.TimeoutMsCeiling != 180000 {
		t.Fatalf("episode timeout ceiling = %d, want 180000", smartCfg.EpisodeRuntime.TimeoutMsCeiling)
	}
}

func newTestSmartRouter(endpoint string) *SmartRouter {
	router := &SmartRouter{
		cfg: Config{
			Enabled:                     true,
			Endpoint:                    endpoint,
			Model:                       "openai/gpt-5.6-sol",
			MaxTokens:                   1000,
			TimeoutMs:                   1000,
			PromptPreviewChars:          2000,
			DecisionHistoryTurns:        defaultDecisionHistoryTurns,
			DecisionHistoryContextChars: defaultDecisionHistoryContextChars,
			CacheTTLSeconds:             300,
			CacheMaxEntries:             10000,
			FallbackModel:               "anthropic/claude-opus-5",
			FallbackPool:                "openrouter",
		},
		menu: []ModelEntry{
			{
				Name:          "z-ai/glm-5.3-flash",
				Pool:          "openrouter",
				InputPrice:    0.07,
				OutputPrice:   0.25,
				Capabilities:  []string{"chat", "code", "reasoning"},
				ContextWindow: 1310720,
			},
			{
				Name:          "anthropic/claude-opus-5",
				Pool:          "openrouter",
				InputPrice:    5.00,
				OutputPrice:   25.00,
				Capabilities:  []string{"chat", "code", "reasoning", "vision"},
				ContextWindow: 200000,
			},
		},
		client:        &http.Client{Timeout: time.Second},
		cache:         NewDecisionCache(10000, 300*time.Second),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		histories:     map[string][]DecisionHistory{},
		controlStates: map[string]*safeControlState{},
		episodes:      map[string]*EpisodeState{},
		sessions:      map[string]*EpisodeSession{},
	}
	router.menuJSON = router.buildMenuText()
	return router
}

func enableSafeControl(router *SmartRouter) {
	router.cfg.SafeControl = SafeControlConfig{
		Enabled:                true,
		RepeatedErrorThreshold: 2,
		PremiumCooldownAfter:   2,
		PremiumCooldownTurns:   1,
		CheapProbeBurstLimit:   3,
	}
}

type fakeTraceQueryer struct {
	traces []plugin.TraceEntry
}

func (q fakeTraceQueryer) QueryTraces(filter plugin.TraceFilter) ([]plugin.TraceEntry, error) {
	var out []plugin.TraceEntry
	for _, trace := range q.traces {
		if filter.EpisodeID != "" && trace.EpisodeID != filter.EpisodeID {
			continue
		}
		if filter.SessionID != "" && trace.SessionID != filter.SessionID {
			continue
		}
		if filter.TrialName != "" && trace.TrialName != filter.TrialName {
			continue
		}
		out = append(out, trace)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

type fakeEpisodeEventQueryer struct {
	events []plugin.EpisodeEvent
}

func (q fakeEpisodeEventQueryer) QueryEpisodeEvents(filter plugin.EpisodeEventFilter) ([]plugin.EpisodeEvent, error) {
	var out []plugin.EpisodeEvent
	for _, event := range q.events {
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
