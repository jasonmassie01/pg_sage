package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/facts"
)

// evid-03a: whether each database has a confirmed owner brief, and the
// accepted and rejected steps in the periods before and after it, from the
// database's metrics refresher (never from the scrape).
//
// No concurrent access tests: the writer reads immutable snapshots; the
// refresher's publication is covered by metrics_refresher_test.go.

func briefView(name string, ok bool, m facts.BriefMeasure) metricsView {
	return metricsView{name: name, sources: []string{metricsSourceFacts},
		snap: &metricsSnapshot{ok: map[string]bool{metricsSourceFacts: ok}, brief: m}}
}

func TestWriteOwnerBriefMetrics_EmptyDeclaresFamiliesOnly(t *testing.T) {
	var b strings.Builder
	writeOwnerBriefMetrics(&b, nil)
	out := b.String()
	for _, f := range []string{"pg_sage_owner_brief_present", "pg_sage_owner_brief_steps"} {
		if !strings.Contains(out, "# TYPE "+f+" gauge") {
			t.Fatalf("family %s is not declared:\n%s", f, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			t.Fatalf("no views rendered a sample %q", line)
		}
	}
	lintScrape(t, out)
}

func TestWriteOwnerBriefMetrics_RendersPresenceAndSteps(t *testing.T) {
	views := []metricsView{
		briefView("app", true, facts.BriefMeasure{Brief: facts.BriefPresent, FactID: 4,
			Before: facts.StepCounts{Accepted: 3, Rejected: 2},
			After:  facts.StepCounts{Accepted: 5, Rejected: 1}}),
		briefView("billing", true, facts.BriefMeasure{Brief: facts.BriefAbsent}),
		briefView("down", false, facts.BriefMeasure{Brief: facts.BriefPresent}),
	}
	var b strings.Builder
	writeOwnerBriefMetrics(&b, views)
	out := b.String()
	for _, want := range []string{
		`pg_sage_owner_brief_present{database="app"} 1`,
		`pg_sage_owner_brief_present{database="billing"} 0`,
		`pg_sage_owner_brief_steps{database="app",period="before",outcome="accepted"} 3`,
		`pg_sage_owner_brief_steps{database="app",period="before",outcome="rejected"} 2`,
		`pg_sage_owner_brief_steps{database="app",period="after",outcome="accepted"} 5`,
		`pg_sage_owner_brief_steps{database="app",period="after",outcome="rejected"} 1`,
		`pg_sage_owner_brief_steps{database="billing",period="after",outcome="accepted"} 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `database="down"`) {
		t.Errorf("a database whose refresh failed rendered a sample:\n%s", out)
	}
	lintScrape(t, out)
}

// Error propagation: a failed read leaves the snapshot's brief unset and
// the source not ok, so the scrape renders nothing for that database.
func TestOwnerBriefMetricsSource_NoPoolIsAnError(t *testing.T) {
	src := ownerBriefMetricsSource(nil)
	if src.name != metricsSourceFacts {
		t.Fatalf("source name %q", src.name)
	}
	var s metricsSnapshot
	if err := src.read(context.Background(), &s); !errors.Is(err, errNoMetricsPool) {
		t.Fatalf("read without a pool: %v, want errNoMetricsPool", err)
	}
	if s.brief.Brief != "" {
		t.Fatalf("a failed read set the brief: %+v", s.brief)
	}
}
