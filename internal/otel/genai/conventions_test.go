package genai

import (
	"encoding/json"
	"testing"
)

func TestParseRequestAttrs(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hello"}],
		"max_tokens": 100,
		"temperature": 0.7,
		"top_p": 0.9,
		"stream": true,
		"stop": ["\n", "END"]
	}`)

	attrs := ParseRequestAttrs(body)
	if attrs == nil {
		t.Fatal("expected non-nil attrs")
	}
	if attrs.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", attrs.Model)
	}
	if attrs.System != "openai" {
		t.Errorf("system = %q, want openai", attrs.System)
	}
	if attrs.Operation != "chat" {
		t.Errorf("operation = %q, want chat", attrs.Operation)
	}
	if attrs.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100", attrs.MaxTokens)
	}
	if attrs.Temperature != 0.7 {
		t.Errorf("temperature = %f, want 0.7", attrs.Temperature)
	}
	if attrs.TopP != 0.9 {
		t.Errorf("top_p = %f, want 0.9", attrs.TopP)
	}
	if !attrs.Stream {
		t.Error("stream = false, want true")
	}
	if len(attrs.StopSequences) != 2 {
		t.Errorf("stop_sequences len = %d, want 2", len(attrs.StopSequences))
	}
}

func TestParseRequestAttrsEmbedding(t *testing.T) {
	body := []byte(`{"model": "text-embedding-3-small", "input": "hello world"}`)
	attrs := ParseRequestAttrs(body)
	if attrs == nil {
		t.Fatal("expected non-nil attrs")
	}
	if attrs.Operation != "embeddings" {
		t.Errorf("operation = %q, want embeddings", attrs.Operation)
	}
}

func TestInferSystem(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"gpt-4o", "openai"},
		{"openai/gpt-4o-mini", "openai"},
		{"claude-3-opus", "anthropic"},
		{"anthropic/claude-3-5-sonnet", "anthropic"},
		{"gemini-1.5-pro", "gcp_gemini"},
		{"google/gemini-2.0-flash", "gcp_gemini"},
		{"mistral-large", "mistralai"},
		{"deepseek-v3", "deepseek"},
		{"qwen2.5-72b", "vllm"},
		{"llama-3.1-70b", "meta"},
		{"unknown-model", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := InferSystem(tt.model)
			if got != tt.want {
				t.Errorf("InferSystem(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestRequestSpanAttrs(t *testing.T) {
	attrs := &RequestAttrs{
		System:      "openai",
		Operation:   "chat",
		Model:       "gpt-4o",
		MaxTokens:   100,
		Temperature: 0.7,
		Stream:      true,
	}

	// Test gen_ai convention
	genAIAttrs := RequestSpanAttrs(attrs, ConvOTelGenAI)
	if len(genAIAttrs) == 0 {
		t.Fatal("expected non-empty gen_ai attrs")
	}
	found := false
	for _, a := range genAIAttrs {
		if string(a.Key) == AttrGenAISystem && a.Value.AsString() == "openai" {
			found = true
		}
	}
	if !found {
		t.Error("gen_ai.system attribute not found")
	}

	// Test openinference convention
	oiAttrs := RequestSpanAttrs(attrs, ConvOpenInference)
	if len(oiAttrs) == 0 {
		t.Fatal("expected non-empty openinference attrs")
	}
	found = false
	for _, a := range oiAttrs {
		if string(a.Key) == AttrLLMVendor && a.Value.AsString() == "openai" {
			found = true
		}
	}
	if !found {
		t.Error("llm.vendor attribute not found")
	}

	// Test both
	bothAttrs := RequestSpanAttrs(attrs, ConvBoth)
	if len(bothAttrs) < len(genAIAttrs) {
		t.Error("both should have at least as many attrs as gen_ai alone")
	}
}

func TestResponseSpanAttrs(t *testing.T) {
	attrs := &ResponseAttrs{
		Model:         "gpt-4o-2024-05-13",
		ResponseID:    "chatcmpl-abc123",
		FinishReasons: []string{"stop"},
		InputTokens:   50,
		OutputTokens:  100,
	}

	genAIAttrs := ResponseSpanAttrs(attrs, ConvOTelGenAI)
	foundTokens := false
	for _, a := range genAIAttrs {
		if string(a.Key) == AttrGenAIUsageInputTokens && a.Value.AsInt64() == 50 {
			foundTokens = true
		}
	}
	if !foundTokens {
		t.Error("gen_ai.usage.input_tokens not found")
	}

	oiAttrs := ResponseSpanAttrs(attrs, ConvOpenInference)
	foundPrompt := false
	for _, a := range oiAttrs {
		if string(a.Key) == AttrLLMTokenCountPrompt && a.Value.AsInt64() == 50 {
			foundPrompt = true
		}
	}
	if !foundPrompt {
		t.Error("llm.token_count.prompt not found")
	}
}

func TestParseResponseAttrs(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc123",
		"model": "gpt-4o-2024-05-13",
		"system_fingerprint": "fp_abc",
		"choices": [{"finish_reason": "stop"}],
		"usage": {
			"prompt_tokens": 50,
			"completion_tokens": 100,
			"total_tokens": 150
		}
	}`)

	attrs := ParseResponseAttrs(body)
	if attrs == nil {
		t.Fatal("expected non-nil attrs")
	}
	if attrs.ResponseID != "chatcmpl-abc123" {
		t.Errorf("id = %q", attrs.ResponseID)
	}
	if attrs.Model != "gpt-4o-2024-05-13" {
		t.Errorf("model = %q", attrs.Model)
	}
	if attrs.SystemFingerprint != "fp_abc" {
		t.Errorf("fingerprint = %q", attrs.SystemFingerprint)
	}
	if attrs.InputTokens != 50 {
		t.Errorf("input_tokens = %d", attrs.InputTokens)
	}
	if attrs.OutputTokens != 100 {
		t.Errorf("output_tokens = %d", attrs.OutputTokens)
	}
	if len(attrs.FinishReasons) != 1 || attrs.FinishReasons[0] != "stop" {
		t.Errorf("finish_reasons = %v", attrs.FinishReasons)
	}
}

func TestParseRequestAttrsEmpty(t *testing.T) {
	if ParseRequestAttrs(nil) != nil {
		t.Error("nil body should return nil")
	}
	if ParseRequestAttrs([]byte{}) != nil {
		t.Error("empty body should return nil")
	}
	if ParseRequestAttrs([]byte("not json")) != nil {
		t.Error("invalid json should return nil")
	}
}

// Ensure json import is used
var _ = json.Marshal
