package advisor

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestWALSystemPrompt_ContainsRules(t *testing.T) {
	checks := []string{
		"max_wal_size",
		"JSON array",
		"ALTER SYSTEM SET",
	}
	for _, want := range checks {
		if !strings.Contains(walSystemPrompt, want) {
			t.Errorf("walSystemPrompt missing expected text %q", want)
		}
	}
}

func TestCountUnloggedTables(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "orders", Relpersistence: "p"},
			{RelName: "cache", Relpersistence: "u"},
			{RelName: "sessions", Relpersistence: "u"},
			{RelName: "temp_data", Relpersistence: "t"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 2 {
		t.Errorf("countUnloggedTables = %d, want 2", got)
	}
}

func TestCountUnloggedTables_None(t *testing.T) {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{
			{RelName: "orders", Relpersistence: "p"},
			{RelName: "items", Relpersistence: "p"},
		},
	}
	got := countUnloggedTables(snap)
	if got != 0 {
		t.Errorf("countUnloggedTables = %d, want 0", got)
	}
}

func TestCountUnloggedTables_EmptySnapshot(t *testing.T) {
	snap := &collector.Snapshot{}
	got := countUnloggedTables(snap)
	if got != 0 {
		t.Errorf("countUnloggedTables = %d, want 0", got)
	}
}

func TestWALSystemPrompt_ContainsAntiThinking(t *testing.T) {
	if !strings.Contains(walSystemPrompt, "No thinking") {
		t.Error("walSystemPrompt missing 'No thinking' anti-thinking directive")
	}
}
