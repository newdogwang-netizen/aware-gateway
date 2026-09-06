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

func TestBuildPromptIncludesCostQualityTurnRiskGuidance(t *testing.T) {
	router := newTestSmartRouter("http://127.0.0.1:1")
	prompt := router.buildPrompt(&parsedRequest{
		Model:           "auto",
		MessageCount:    4,
		EstimatedTokens: 200,
		LatestUserMsg:   "Inspect installed packages for a security-related task.",
	}, "1. model=anthropic/claude-opus-5 turn=critical_hypothesis state=forming critical=true recover=hard ctx=\"root cause unclear\" reason=\"need path setting\"")

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
		"context_summary",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
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
	if decision, err := router.Route(req, completionBody); err != nil {
		t.Fatalf("completion Route returned error: %v", err)
	} else if decision == nil || decision.Model != "anthropic/claude-opus-5" {
		t.Fatalf("completion Route = %#v, want Opus", decision)
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
	}
}
