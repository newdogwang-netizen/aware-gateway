package smartrouter

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func newTestSmartRouter(endpoint string) *SmartRouter {
	router := &SmartRouter{
		cfg: Config{
			Enabled:            true,
			Endpoint:           endpoint,
			Model:              "qwen3.8-27b",
			MaxTokens:          100,
			TimeoutMs:          1000,
			PromptPreviewChars: 2000,
			CacheTTLSeconds:    300,
			CacheMaxEntries:    10000,
			FallbackModel:      "anthropic/claude-opus-5",
			FallbackPool:       "openrouter",
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
				Name:          "openai/gpt-5.6-sol",
				Pool:          "openrouter",
				InputPrice:    2.00,
				OutputPrice:   10.00,
				Capabilities:  []string{"chat", "code", "reasoning"},
				ContextWindow: 1050000,
			},
		},
		client: &http.Client{Timeout: time.Second},
		cache:  NewDecisionCache(10000, 300*time.Second),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	router.menuJSON = router.buildMenuText()
	return router
}
