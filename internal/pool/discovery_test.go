package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aware/gateway/internal/config"
)

// helper: create a pool with N endpoints, each serving given models
func newTestPool(t *testing.T, name string, endpoints []struct {
	name   string
	models []string
}) *Pool {
	t.Helper()
	var eps []config.EndpointConfig
	for _, e := range endpoints {
		eps = append(eps, config.EndpointConfig{
			Name:       e.name,
			URL:        "http://localhost:0",
			HealthPath: "/health",
			Weight:     10,
			Models:     e.models,
		})
	}
	p, err := NewPool(name, config.PoolConfig{
		Strategy:  "round_robin",
		Endpoints: eps,
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServesModel(t *testing.T) {
	ep := &Endpoint{
		Name:   "test-ep",
		Models: []string{"gpt-4o", "gpt-4o-mini"},
	}

	// Explicit model match
	if !ep.ServesModel("gpt-4o") {
		t.Error("expected ServesModel(gpt-4o) = true")
	}
	if !ep.ServesModel("gpt-4o-mini") {
		t.Error("expected ServesModel(gpt-4o-mini) = true")
	}

	// Model not served
	if ep.ServesModel("claude-3") {
		t.Error("expected ServesModel(claude-3) = false")
	}

	// Empty model → true (no filter)
	if !ep.ServesModel("") {
		t.Error("expected ServesModel('') = true (no filter)")
	}

	// Endpoint with no model info → serves everything
	ep2 := &Endpoint{Name: "no-info"}
	if !ep2.ServesModel("anything") {
		t.Error("endpoint with no model info should serve everything")
	}
}

func TestServesModelDiscovered(t *testing.T) {
	ep := &Endpoint{
		Name:   "test-ep",
		Models: []string{"gpt-4o"},
	}
	// Discovered models augment static config
	ep.SetDiscoveredModels([]string{"gpt-4.1", "o3-mini"})

	if !ep.ServesModel("gpt-4o") {
		t.Error("static model gpt-4o should be served")
	}
	if !ep.ServesModel("gpt-4.1") {
		t.Error("discovered model gpt-4.1 should be served")
	}
	if !ep.ServesModel("o3-mini") {
		t.Error("discovered model o3-mini should be served")
	}
	if ep.ServesModel("claude-3") {
		t.Error("claude-3 should not be served")
	}
}

func TestAllModels(t *testing.T) {
	ep := &Endpoint{
		Name:   "test-ep",
		Models: []string{"gpt-4o", "gpt-4o-mini"},
	}
	ep.SetDiscoveredModels([]string{"gpt-4o", "gpt-4.1"}) // gpt-4o is duplicate

	all := ep.AllModels()
	// Should be deduplicated and sorted
	if len(all) != 3 {
		t.Fatalf("AllModels len = %d, want 3 (deduped)", len(all))
	}
	expected := []string{"gpt-4.1", "gpt-4o", "gpt-4o-mini"}
	for i, m := range all {
		if m != expected[i] {
			t.Errorf("AllModels[%d] = %q, want %q", i, m, expected[i])
		}
	}
}

func TestNextForModel(t *testing.T) {
	p := newTestPool(t, "multi-model", []struct {
		name   string
		models []string
	}{
		{"ep-gpt4", []string{"gpt-4o", "gpt-4o-mini"}},
		{"ep-claude", []string{"claude-3-opus", "claude-3-sonnet"}},
		{"ep-llama", []string{"llama-3.1-70b"}},
	})

	// All endpoints start healthy
	for _, ep := range p.Endpoints {
		ep.SetHealthy(true)
	}

	// Select for gpt-4o → should only pick ep-gpt4
	ep := p.NextForModel("gpt-4o")
	if ep == nil {
		t.Fatal("expected non-nil endpoint")
	}
	if ep.Name != "ep-gpt4" {
		t.Errorf("NextForModel(gpt-4o) = %q, want ep-gpt4", ep.Name)
	}

	// Select for claude-3-opus → should only pick ep-claude
	ep = p.NextForModel("claude-3-opus")
	if ep == nil {
		t.Fatal("expected non-nil endpoint")
	}
	if ep.Name != "ep-claude" {
		t.Errorf("NextForModel(claude-3-opus) = %q, want ep-claude", ep.Name)
	}

	// Select for llama → ep-llama
	ep = p.NextForModel("llama-3.1-70b")
	if ep == nil || ep.Name != "ep-llama" {
		t.Errorf("NextForModel(llama) = %v, want ep-llama", ep)
	}

	// Select for unknown model → fallback to Next() (any healthy)
	ep = p.NextForModel("unknown-model")
	if ep == nil {
		t.Fatal("expected fallback to Next() for unknown model")
	}

	// Empty model → delegates to Next()
	ep = p.NextForModel("")
	if ep == nil {
		t.Fatal("expected non-nil for empty model")
	}
}

func TestNextForModelWithUnhealthy(t *testing.T) {
	p := newTestPool(t, "failover", []struct {
		name   string
		models []string
	}{
		{"ep1", []string{"gpt-4o"}},
		{"ep2", []string{"gpt-4o"}},
	})

	// Both healthy, both serve gpt-4o
	p.Endpoints[0].SetHealthy(true)
	p.Endpoints[1].SetHealthy(true)

	// Mark ep1 unhealthy
	p.Endpoints[0].SetHealthy(false)

	ep := p.NextForModel("gpt-4o")
	if ep == nil {
		t.Fatal("expected non-nil")
	}
	if ep.Name != "ep2" {
		t.Errorf("NextForModel(gpt-4o) with ep1 down = %q, want ep2", ep.Name)
	}
}

func TestNextForModelAllUnhealthy(t *testing.T) {
	p := newTestPool(t, "all-down", []struct {
		name   string
		models []string
	}{
		{"ep1", []string{"gpt-4o"}},
	})
	p.Endpoints[0].SetHealthy(false)

	// All unhealthy → NextForModel falls back to Next() which returns
	// the (unhealthy) endpoint rather than nil
	ep := p.NextForModel("gpt-4o")
	if ep == nil {
		t.Fatal("expected fallback to Next() returning unhealthy endpoint")
	}
}

func TestNextForModelLeastConn(t *testing.T) {
	p := newTestPool(t, "least-conn", []struct {
		name   string
		models []string
	}{
		{"ep1", []string{"gpt-4o"}},
		{"ep2", []string{"gpt-4o"}},
	})
	p.Strategy = "least_conn"
	p.Endpoints[0].SetHealthy(true)
	p.Endpoints[1].SetHealthy(true)

	// Give ep1 more in-flight requests
	p.Endpoints[0].Inc()
	p.Endpoints[0].Inc()
	// ep2 has 0 in-flight

	ep := p.NextForModel("gpt-4o")
	if ep == nil {
		t.Fatal("expected non-nil")
	}
	if ep.Name != "ep2" {
		t.Errorf("least_conn should pick ep2 (0 in-flight), got %s (%d in-flight)",
			ep.Name, ep.InFlight())
	}
}

func TestPoolAllModels(t *testing.T) {
	p := newTestPool(t, "multi", []struct {
		name   string
		models []string
	}{
		{"ep1", []string{"gpt-4o", "gpt-4o-mini"}},
		{"ep2", []string{"claude-3"}},
	})

	all := p.AllModels()
	if len(all) != 3 {
		t.Fatalf("AllModels len = %d, want 3", len(all))
	}
	// Should be sorted
	expected := []string{"claude-3", "gpt-4o", "gpt-4o-mini"}
	for i, m := range all {
		if m != expected[i] {
			t.Errorf("AllModels[%d] = %q, want %q", i, m, expected[i])
		}
	}
}

// --- API Auto-Discovery Tests ---

func TestDiscoverModels(t *testing.T) {
	// Mock /v1/models endpoint
	var callCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]string{
					{"id": "gpt-4o"},
					{"id": "gpt-4o-mini"},
					{"id": "gpt-4.1"},
				},
			})
			return
		}
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	p, err := NewPool("test", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{
				Name:       "mock-ep",
				URL:        server.URL,
				HealthPath: "/health",
				Weight:     10,
			},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// Initially no discovered models
	models := p.Endpoints[0].AllModels()
	if len(models) != 0 {
		t.Fatalf("initial AllModels = %v, want empty", models)
	}

	// Run discovery (synchronously)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.discoverModels(ctx, 5*time.Second)

	// Wait for discovery to complete
	discovered, _ := p.Endpoints[0].discoveredModels.Load().([]string)
	if len(discovered) != 3 {
		t.Fatalf("discovered models = %v, want 3", discovered)
	}

	// AllModels should now include discovered models
	allModels := p.Endpoints[0].AllModels()
	if len(allModels) != 3 {
		t.Fatalf("AllModels after discovery = %v, want 3", allModels)
	}

	// ServesModel should work for discovered models
	if !p.Endpoints[0].ServesModel("gpt-4o") {
		t.Error("ServesModel(gpt-4o) should be true after discovery")
	}
	if !p.Endpoints[0].ServesModel("gpt-4.1") {
		t.Error("ServesModel(gpt-4.1) should be true after discovery")
	}
}

func TestDiscoverModelsWithStatic(t *testing.T) {
	// Endpoint has static models AND API discovery
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]string{
					{"id": "gpt-4o"},
					{"id": "gpt-4.1"},
				},
			})
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()

	p, err := NewPool("test", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{
				Name:       "mock-ep",
				URL:        server.URL,
				HealthPath: "/health",
				Weight:     10,
				Models:     []string{"gpt-4o", "gpt-4o-mini"}, // static
			},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.discoverModels(ctx, 5*time.Second)

	// AllModels = union of static + discovered (deduped)
	all := p.Endpoints[0].AllModels()
	// gpt-4o appears in both → deduped. Total: gpt-4o, gpt-4o-mini, gpt-4.1 = 3
	if len(all) != 3 {
		t.Fatalf("AllModels = %v (len %d), want 3 (deduped)", all, len(all))
	}
}

func TestDiscoverModels404(t *testing.T) {
	// Endpoint that doesn't support /v1/models (e.g. Whisper ASR)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer server.Close()

	p, err := NewPool("test", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{
				Name:       "whisper-ep",
				URL:        server.URL,
				HealthPath: "/health",
				Weight:     10,
				Models:     []string{"whisper-large-v3"}, // static only
			},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.discoverModels(ctx, 5*time.Second)

	// Should keep static models, no discovered models
	all := p.Endpoints[0].AllModels()
	if len(all) != 1 || all[0] != "whisper-large-v3" {
		t.Errorf("AllModels = %v, want [whisper-large-v3] (static only)", all)
	}
}

func TestDiscoverModelsAuth(t *testing.T) {
	// Endpoint that requires auth for /v1/models
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/v1/models" {
			if gotAuth == "" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]string{
					{"id": "gpt-4o"},
				},
			})
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()

	p, err := NewPool("test", config.PoolConfig{
		Strategy: "round_robin",
		Endpoints: []config.EndpointConfig{
			{
				Name:       "auth-ep",
				URL:        server.URL,
				HealthPath: "/health",
				Weight:     10,
				AuthToken:  "test-key-123",
			},
		},
	}, config.CircuitBreakerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.discoverModels(ctx, 5*time.Second)

	if gotAuth != "Bearer test-key-123" {
		t.Errorf("discovery should send auth token, got %q", gotAuth)
	}

	discovered, _ := p.Endpoints[0].discoveredModels.Load().([]string)
	if len(discovered) != 1 || discovered[0] != "gpt-4o" {
		t.Errorf("discovered = %v, want [gpt-4o]", discovered)
	}
}

func TestGetStatusIncludesModels(t *testing.T) {
	p := newTestPool(t, "status", []struct {
		name   string
		models []string
	}{
		{"ep1", []string{"gpt-4o", "gpt-4o-mini"}},
		{"ep2", []string{"claude-3"}},
	})

	status := p.GetStatus()

	// Pool-level all_models
	if len(status.AllModels) != 3 {
		t.Errorf("AllModels = %v, want 3", status.AllModels)
	}

	// Per-endpoint models
	if len(status.Endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(status.Endpoints))
	}
	if len(status.Endpoints[0].Models) != 2 {
		t.Errorf("ep1 models = %v, want 2", status.Endpoints[0].Models)
	}
}

// --- End of tests ---
