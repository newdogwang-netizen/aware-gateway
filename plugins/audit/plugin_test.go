package audit

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aware/gateway/internal/plugin"
)

func TestStoreRecordsAndQueriesRouteBudget(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	store.Record(Record{
		TraceID:        "trace-1",
		Timestamp:      time.Now(),
		Method:         "POST",
		Path:           "/v1/chat/completions",
		Status:         200,
		Model:          "auto",
		RoutedModel:    "z-ai/glm-5.3-flash",
		Pool:           "openrouter",
		BudgetAction:   "cheap_probe",
		RouteMaxTokens: 1234,
		RouteTimeoutMs: 45000,
		EpisodeID:      "episode-1",
		EpisodeOp:      "continue",
		StateVersion:   3,
		StateBefore:    `{"state_version":3}`,
		StateAfter:     `{"state_version":4}`,
		SessionID:      "trial-1__agent",
	})
	store.Flush()

	traces, err := store.QueryTraces(plugin.TraceFilter{SessionID: "trial-1__agent"})
	if err != nil {
		t.Fatalf("QueryTraces returned error: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(traces))
	}
	if traces[0].BudgetAction != "cheap_probe" {
		t.Fatalf("budget action = %q, want cheap_probe", traces[0].BudgetAction)
	}
	if traces[0].RouteMaxTokens != 1234 {
		t.Fatalf("route max tokens = %d, want 1234", traces[0].RouteMaxTokens)
	}
	if traces[0].RouteTimeoutMs != 45000 {
		t.Fatalf("route timeout ms = %d, want 45000", traces[0].RouteTimeoutMs)
	}
	if traces[0].EpisodeID != "episode-1" {
		t.Fatalf("episode id = %q, want episode-1", traces[0].EpisodeID)
	}
	if traces[0].EpisodeOp != "continue" {
		t.Fatalf("episode operation = %q, want continue", traces[0].EpisodeOp)
	}
	if traces[0].StateVersion != 3 {
		t.Fatalf("episode state version = %d, want 3", traces[0].StateVersion)
	}
	if traces[0].StateBefore != `{"state_version":3}` {
		t.Fatalf("episode state before = %q", traces[0].StateBefore)
	}
	if traces[0].StateAfter != `{"state_version":4}` {
		t.Fatalf("episode state after = %q", traces[0].StateAfter)
	}

	episodeTraces, err := store.QueryTraces(plugin.TraceFilter{EpisodeID: "episode-1"})
	if err != nil {
		t.Fatalf("QueryTraces by episode returned error: %v", err)
	}
	if len(episodeTraces) != 1 || episodeTraces[0].TraceID != "trace-1" {
		t.Fatalf("episode traces = %#v, want trace-1", episodeTraces)
	}
}
