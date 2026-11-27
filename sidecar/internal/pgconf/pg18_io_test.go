package pgconf

import (
	"strings"
	"testing"
)

// market-15: PostgreSQL 18's I/O settings are documented for the advisor's
// inputs only. None of them is proposable or executable, so the docs add
// no write path.

// No concurrent access tests: AdviceOnlyDocs is a read-only package map.

var pg18IONames = []string{"io_method", "io_workers", "io_combine_limit",
	"maintenance_io_concurrency"}

func TestAdviceOnlyDocsCoverThePG18IOSettings(t *testing.T) {
	if len(AdviceOnlyDocs) != len(pg18IONames) {
		t.Errorf("AdviceOnlyDocs has %d entries, want the %d I/O settings",
			len(AdviceOnlyDocs), len(pg18IONames))
	}
	for _, name := range pg18IONames {
		doc, ok := AdviceOnlyDocs[name]
		if !ok {
			t.Errorf("AdviceOnlyDocs lacks %s", name)
			continue
		}
		if strings.TrimSpace(doc.Description) == "" {
			t.Errorf("%s has no description", name)
		}
		if len(doc.NotesFor(18)) == 0 {
			t.Errorf("%s has no note for PostgreSQL 18", name)
		}
		if doc.NotesFor(0) != nil {
			t.Errorf("%s has notes for an unknown version: %v", name, doc.NotesFor(0))
		}
	}
}

// The settings that only exist on 18 carry no note on 17, so a PG17
// prompt never describes a setting the server lacks.
func TestPG18OnlySettingsHaveNoNoteBefore18(t *testing.T) {
	for _, name := range []string{"io_method", "io_workers"} {
		if got := AdviceOnlyDocs[name].NotesFor(17); got != nil {
			t.Errorf("%s notes on 17 = %v, want none", name, got)
		}
	}
	method := strings.Join(AdviceOnlyDocs["io_method"].NotesFor(18), " ")
	for _, want := range []string{"sync", "worker", "io_uring", "restart", "liburing"} {
		if !strings.Contains(method, want) {
			t.Errorf("io_method note %q does not mention %q", method, want)
		}
	}
	workers := strings.Join(AdviceOnlyDocs["io_workers"].NotesFor(18), " ")
	if !strings.Contains(workers, "io_method = worker") {
		t.Errorf("io_workers note %q does not say it applies only to worker", workers)
	}
}

func TestAdviceOnlyDocsAreNotProposableOrExecutable(t *testing.T) {
	for _, name := range pg18IONames {
		if AdvisorGUC(name) {
			t.Errorf("AdvisorGUC(%q) = true, want false (advice only)", name)
		}
		if ExecutableGUC(name) {
			t.Errorf("ExecutableGUC(%q) = true, want false (advice only)", name)
		}
		if AutonomousGUC(name) {
			t.Errorf("AutonomousGUC(%q) = true, want false", name)
		}
		if _, inDocs := Docs[name]; inDocs {
			t.Errorf("%s is in Docs: Docs must stay the executable list", name)
		}
	}
	for name := range AdviceOnlyDocs {
		if _, inDocs := Docs[name]; inDocs {
			t.Errorf("AdviceOnlyDocs key %s overlaps Docs", name)
		}
	}
}

func TestIORestartList(t *testing.T) {
	cases := map[string]bool{
		"io_method":                  true,
		"  IO_METHOD ":               true,
		"io_workers":                 false,
		"io_combine_limit":           false,
		"maintenance_io_concurrency": false,
		"effective_io_concurrency":   false,
	}
	for name, want := range cases {
		if got := RequiresRestart(name); got != want {
			t.Errorf("RequiresRestart(%q) = %v, want %v", name, got, want)
		}
	}
}

// The effective_io_concurrency entry keeps its safe range: market-15 must
// not widen what the executor may set.
func TestEffectiveIOConcurrencyRangeUnchanged(t *testing.T) {
	d, ok := Docs["effective_io_concurrency"]
	if !ok || d.SafeMin != 0 || d.SafeMax != 1000 || d.Unit != "count" {
		t.Fatalf("effective_io_concurrency doc = %+v, want count 0..1000", d)
	}
}
