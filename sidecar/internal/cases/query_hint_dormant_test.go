package cases

import (
	"strings"
	"testing"
	"time"
)

// A dormant hint (tuning-01) is installed but its query has not run since
// the last revalidation. Its case must say so, stay informational and
// offer no retire action: dormant is not broken.
func TestProjectDormantQueryHintIsInformational(t *testing.T) {
	hint := SourceQueryHint{
		QueryID:      777,
		DatabaseName: "prod",
		HintText:     `Set(work_mem "64MB")`,
		Symptom:      "disk sort",
		Status:       "dormant",
		CreatedAt:    time.Now().UTC(),
	}

	got := ProjectQueryHint(hint)

	if got.Title != "Query hint dormant for query 777" {
		t.Fatalf("Title = %q", got.Title)
	}
	if !strings.Contains(got.WhyNow, "installed") ||
		!strings.Contains(got.WhyNow, "not run") {
		t.Fatalf("WhyNow = %q, want it to say the hint is installed and idle", got.WhyNow)
	}
	if got.Severity != SeverityInfo {
		t.Fatalf("Severity = %q, want info", got.Severity)
	}
	if len(got.ActionCandidates) != 0 {
		t.Fatalf("ActionCandidates = %+v, want none for a dormant hint", got.ActionCandidates)
	}
}

// Revalidation no longer retires hints; a retired hint came from the
// retire_query_hint action, and its case must not blame revalidation.
func TestProjectRetiredQueryHintWhyNowNamesTheAction(t *testing.T) {
	got := ProjectQueryHint(SourceQueryHint{
		QueryID: 778, DatabaseName: "prod", HintText: "SeqScan(t)",
		Status: "retired", CreatedAt: time.Now().UTC(),
	})
	if strings.Contains(got.WhyNow, "revalidation") {
		t.Fatalf("WhyNow = %q, want no claim that revalidation retired it", got.WhyNow)
	}
	if !strings.Contains(got.WhyNow, "retire_query_hint") {
		t.Fatalf("WhyNow = %q, want it to name the retire_query_hint action", got.WhyNow)
	}
}
