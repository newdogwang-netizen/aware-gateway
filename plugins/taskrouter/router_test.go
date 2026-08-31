package taskrouter

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/aware/gateway/internal/config"
	"github.com/aware/gateway/internal/pool"
)

// --- Mock Pool Provider ---

type mockPoolProvider struct {
	pools map[string]*pool.Pool
}

func newMockPoolProvider(poolNames ...string) *mockPoolProvider {
	m := &mockPoolProvider{pools: make(map[string]*pool.Pool)}
	for _, name := range poolNames {
		pcfg := config.PoolConfig{
			Strategy: "round_robin",
			Endpoints: []config.EndpointConfig{
				{Name: "ep1", URL: "http://localhost:0", HealthPath: "/health", Weight: 10},
			},
		}
		p, err := pool.NewPool(name, pcfg, config.CircuitBreakerConfig{})
		if err != nil {
			panic(err)
		}
		// Mark endpoint as healthy (default is true, but ensure)
		p.Endpoints[0].SetHealthy(true)
		m.pools[name] = p
	}
	return m
}

func (m *mockPoolProvider) Get(name string) (*pool.Pool, bool) {
	p, ok := m.pools[name]
	return p, ok
}

func (m *mockPoolProvider) All() map[string]*pool.Pool {
	out := make(map[string]*pool.Pool, len(m.pools))
	for k, v := range m.pools {
		out[k] = v
	}
	return out
}

// Also satisfy plugin.PoolProvider via duck typing
// (the interface matches exactly)

// --- Tests ---

func TestClassifyChat(t *testing.T) {
	c := NewTaskClassifier()
	p := &ParsedRequest{
		Messages: []Message{
			{Role: "user", Content: "Hello, how are you?"},
		},
	}
	if task := c.Classify(p); task != TaskChat {
		t.Errorf("expected chat, got %s", task)
	}
}

func TestClassifyCode(t *testing.T) {
	c := NewTaskClassifier()
	tests := []string{
		"Write a function to sort an array",
		"Debug this error trace: panic at line 42",
		"Refactor the Python class to use composition",
		"Write a unit test for the API endpoint",
	}
	for _, text := range tests {
		p := &ParsedRequest{
			Messages: []Message{
				{Role: "user", Content: text},
			},
		}
		if task := c.Classify(p); task != TaskCode {
			t.Errorf("expected code for %q, got %s", text, task)
		}
	}
}

func TestClassifyReasoning(t *testing.T) {
	c := NewTaskClassifier()
	tests := []string{
		"Analyze why the system failed and explain step by step",
		"Let's think about this logically and derive the answer",
		"Prove that the sum of two odds is even",
	}
	for _, text := range tests {
		p := &ParsedRequest{
			Messages: []Message{
				{Role: "user", Content: text},
			},
		}
		if task := c.Classify(p); task != TaskReasoning {
			t.Errorf("expected reasoning for %q, got %s", text, task)
		}
	}
}

func TestClassifyVision(t *testing.T) {
	c := NewTaskClassifier()
	content := []any{
		map[string]any{
			"type": "text",
			"text": "What's in this image?",
		},
		map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/png;base64,abc"},
		},
	}
	p := &ParsedRequest{
		Messages: []Message{
			{Role: "user", Content: content},
		},
	}
	if task := c.Classify(p); task != TaskVision {
		t.Errorf("expected vision, got %s", task)
	}
}

func TestParseRequest(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are helpful"},
			{"role": "user", "content": "Hello"}
		],
		"max_tokens": 50,
		"stream": true
	}`)

	p := parseRequest(body, "application/json")
	if p == nil {
		t.Fatal("expected non-nil parsed request")
	}
	if p.Model != "gpt-4o" {
		t.Errorf("model = %q", p.Model)
	}
	if len(p.Messages) != 2 {
		t.Errorf("messages len = %d, want 2", len(p.Messages))
	}
	if !p.IsStream {
		t.Error("stream = false, want true")
	}
	if p.EstimatedTokens <= 0 {
		t.Error("estimated_tokens should be positive")
	}
}

func TestParseRequestEmbedding(t *testing.T) {
	body := []byte(`{"model": "text-embedding-3-small", "input": "hello"}`)
	p := parseRequest(body, "application/json")
	if p == nil {
		t.Fatal("expected non-nil")
	}
	if !p.IsEmbedding {
		t.Error("expected embedding=true")
	}
}

func TestModelRegistryCandidates(t *testing.T) {
	reg := NewModelRegistry()
	reg.Register(&ModelInfo{
		Name:          "gpt-4o",
		Pool:          "openai",
		Capabilities:  []string{"chat", "code", "reasoning", "vision"},
		ContextWindow: 128000,
	})
	reg.Register(&ModelInfo{
		Name:          "gpt-4o-mini",
		Pool:          "openai",
		Capabilities:  []string{"chat"},
		ContextWindow: 128000,
		InputPrice:    0.15,
		OutputPrice:   0.60,
	})
	reg.Register(&ModelInfo{
		Name:          "claude-3-haiku",
		Pool:          "anthropic",
		Capabilities:  []string{"chat", "code"},
		ContextWindow: 200000,
		InputPrice:    0.25,
		OutputPrice:   1.25,
	})

	if reg.Size() != 3 {
		t.Errorf("registry size = %d, want 3", reg.Size())
	}

	pp := newMockPoolProvider("openai", "anthropic")

	// All models should be candidates for "chat" task with small token count
	candidates := reg.Candidates(TaskChat, 1000, pp)
	if len(candidates) != 3 {
		t.Errorf("candidates for chat = %d, want 3", len(candidates))
	}

	// Only vision-capable models for vision task
	candidates = reg.Candidates(TaskVision, 1000, pp)
	if len(candidates) != 1 {
		t.Errorf("candidates for vision = %d, want 1", len(candidates))
	}
	if candidates[0].Name != "gpt-4o" {
		t.Errorf("vision candidate = %q, want gpt-4o", candidates[0].Name)
	}

	// No candidates for a task no model supports
	candidates = reg.Candidates(TaskType("audio_gen"), 1000, pp)
	if len(candidates) != 0 {
		t.Errorf("candidates for audio_gen = %d, want 0", len(candidates))
	}

	// Context window filter
	candidates = reg.Candidates(TaskChat, 200000, pp)
	if len(candidates) != 1 {
		t.Errorf("candidates for 200k tokens = %d, want 1 (only claude)", len(candidates))
	}
}

func TestScorerSelectCheapest(t *testing.T) {
	s := NewScorer(false)
	candidates := []*ModelInfo{
		{Name: "gpt-4o", Pool: "openai", InputPrice: 2.50, OutputPrice: 10.00},
		{Name: "gpt-4o-mini", Pool: "openai", InputPrice: 0.15, OutputPrice: 0.60},
		{Name: "claude-3-haiku", Pool: "anthropic", InputPrice: 0.25, OutputPrice: 1.25},
	}
	selected := s.Select(candidates, StratCheapest, newMockPoolProvider("openai", "anthropic"))
	if selected == nil {
		t.Fatal("expected non-nil selection")
	}
	if selected.Name != "gpt-4o-mini" {
		t.Errorf("cheapest = %q, want gpt-4o-mini", selected.Name)
	}
}

func TestScorerSelectBestQuality(t *testing.T) {
	s := NewScorer(false)
	candidates := []*ModelInfo{
		{Name: "gpt-4o-mini", Pool: "openai", Capabilities: []string{"chat"}, ContextWindow: 128000},
		{Name: "gpt-4o", Pool: "openai", Capabilities: []string{"chat", "code", "reasoning", "vision"}, ContextWindow: 128000},
		{Name: "claude-3-haiku", Pool: "anthropic", Capabilities: []string{"chat", "code"}, ContextWindow: 200000},
	}
	selected := s.Select(candidates, StratBestQuality, newMockPoolProvider())
	if selected == nil {
		t.Fatal("expected non-nil selection")
	}
	// claude-3-haiku has largest context window
	if selected.Name != "claude-3-haiku" {
		t.Errorf("best_quality = %q, want claude-3-haiku", selected.Name)
	}
}

func TestScorerSelectAny(t *testing.T) {
	s := NewScorer(false)
	candidates := []*ModelInfo{
		{Name: "a", Pool: "p1"},
		{Name: "b", Pool: "p2"},
	}
	selected := s.Select(candidates, StratAny, newMockPoolProvider())
	if selected == nil || selected.Name != "a" {
		t.Errorf("any should return first candidate")
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello, can you help me?"},
	}
	tokens := estimateTokens(msgs, 100)
	if tokens <= 0 {
		t.Error("expected positive token estimate")
	}
	if tokens < 100 || tokens > 200 {
		t.Errorf("token estimate = %d, expected 100-200", tokens)
	}
}

// Ensure imports are used
var _ = json.Marshal
var _ = httptest.NewServer
