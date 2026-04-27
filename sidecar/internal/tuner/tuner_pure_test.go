package tuner

import (
	"strings"
	"testing"
)

func TestSymptomNames(t *testing.T) {
	symptoms := []PlanSymptom{
		{Kind: SymptomDiskSort},
		{Kind: SymptomHashSpill},
		{Kind: SymptomHighPlanTime},
	}
	names := symptomNames(symptoms)
	if len(names) != 3 {
		t.Fatalf("got %d names, want 3", len(names))
	}
	if names[0] != string(SymptomDiskSort) {
		t.Errorf("names[0] = %q", names[0])
	}
	if names[1] != string(SymptomHashSpill) {
		t.Errorf("names[1] = %q", names[1])
	}
	if names[2] != string(SymptomHighPlanTime) {
		t.Errorf("names[2] = %q", names[2])
	}
}

func TestSymptomNames_Empty(t *testing.T) {
	names := symptomNames(nil)
	if len(names) != 0 {
		t.Errorf("got %d names, want 0", len(names))
	}
}

func TestBuildTitle_SingleSymptom(t *testing.T) {
	tests := []struct {
		kind SymptomKind
		want string
	}{
		{SymptomDiskSort, "work_mem"},
		{SymptomHashSpill, "work_mem"},
		{SymptomHighPlanTime, "generic plan"},
		{SymptomBadNestedLoop, "nested loop"},
		{SymptomSeqScanWithIndex, "index scan"},
		{SymptomParallelDisabled, "parallel"},
	}
	for _, tt := range tests {
		title := buildTitle([]PlanSymptom{{Kind: tt.kind}})
		if !strings.Contains(title, tt.want) {
			t.Errorf("buildTitle(%s) = %q, want to contain %q",
				tt.kind, title, tt.want)
		}
	}
}

func TestBuildTitle_UnknownSymptom(t *testing.T) {
	title := buildTitle([]PlanSymptom{
		{Kind: SymptomKind("unknown_symptom")},
	})
	if title != "Per-query tuning recommendation" {
		t.Errorf("got %q, want default title", title)
	}
}

func TestBuildTitle_MultipleSymptoms(t *testing.T) {
	symptoms := []PlanSymptom{
		{Kind: SymptomDiskSort},
		{Kind: SymptomHighPlanTime},
	}
	title := buildTitle(symptoms)
	if !strings.Contains(title, "work_mem") {
		t.Errorf("missing work_mem in %q", title)
	}
	if !strings.Contains(title, "generic plan") {
		t.Errorf("missing generic plan in %q", title)
	}
	if !strings.Contains(title, "+") {
		t.Errorf("missing + separator in %q", title)
	}
}

func TestMultiSymptomTitle_AllKinds(t *testing.T) {
	symptoms := []PlanSymptom{
		{Kind: SymptomDiskSort},
		{Kind: SymptomHashSpill},
		{Kind: SymptomHighPlanTime},
		{Kind: SymptomBadNestedLoop},
		{Kind: SymptomSeqScanWithIndex},
		{Kind: SymptomParallelDisabled},
	}
	title := multiSymptomTitle(symptoms)
	for _, want := range []string{
		"work_mem", "generic plan", "join strategy",
		"index scan", "parallel workers",
	} {
		if !strings.Contains(title, want) {
			t.Errorf("missing %q in %q", want, title)
		}
	}
}

func TestMultiSymptomTitle_NoRecognizedKinds(t *testing.T) {
	symptoms := []PlanSymptom{
		{Kind: SymptomKind("future_symptom")},
	}
	title := multiSymptomTitle(symptoms)
	if title != "Per-query tuning recommendation" {
		t.Errorf("got %q, want default", title)
	}
}

func TestMultiSymptomTitle_DedupsDiskSortAndHashSpill(t *testing.T) {
	symptoms := []PlanSymptom{
		{Kind: SymptomDiskSort},
		{Kind: SymptomHashSpill},
	}
	title := multiSymptomTitle(symptoms)
	// Both map to work_mem, should appear once
	if strings.Count(title, "work_mem") != 1 {
		t.Errorf("work_mem should appear once in %q", title)
	}
}

func TestBuildRationale(t *testing.T) {
	prescriptions := []Prescription{
		{Rationale: "reduce disk sorts"},
		{Rationale: "avoid hash spills"},
	}
	got := buildRationale(prescriptions)
	if got != "reduce disk sorts; avoid hash spills" {
		t.Errorf("got %q", got)
	}
}

func TestBuildRationale_Empty(t *testing.T) {
	got := buildRationale(nil)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestBuildRationale_Single(t *testing.T) {
	got := buildRationale([]Prescription{
		{Rationale: "only one"},
	})
	if got != "only one" {
		t.Errorf("got %q", got)
	}
}

func TestBuildInsertSQL(t *testing.T) {
	sql := BuildInsertSQL("SELECT * FROM foo", "SET work_mem = '256MB'")
	if sql == "" {
		t.Fatal("BuildInsertSQL returned empty string")
	}
	if !strings.Contains(sql, "norm_query_string") {
		t.Errorf("should contain norm_query_string column")
	}
	if !strings.Contains(sql, "SELECT * FROM foo") {
		t.Errorf("should contain query text")
	}
	if !strings.Contains(sql, "work_mem") {
		t.Errorf("should contain hint SQL")
	}
}

func TestBuildInsertSQL_DollarQuotesSingleQuotes(t *testing.T) {
	// Dollar-quoted literals do not process single quotes or
	// backslashes, so single quotes pass through unmodified and
	// the result is injection-safe.
	sql := BuildInsertSQL("SELECT 1", "it's a hint")
	if !strings.Contains(sql, "it's a hint") {
		t.Errorf("hint text missing from dollar-quoted body: %s", sql)
	}
	if !strings.Contains(sql, "$sageqh$") {
		t.Errorf("expected $sageqh$ dollar tag: %s", sql)
	}
}

func TestBuildInsertSQL_DollarQuotesQueryTextQuotes(t *testing.T) {
	sql := BuildInsertSQL("SELECT * FROM t WHERE name = 'foo'", "hint")
	if !strings.Contains(sql, "name = 'foo'") {
		t.Errorf("query text quotes altered inside dollar-quote: %s", sql)
	}
	if !strings.Contains(sql, "$sageqh$") {
		t.Errorf("expected $sageqh$ dollar tag: %s", sql)
	}
}

func TestBuildDeleteSQL_DollarQuotesQueryTextQuotes(t *testing.T) {
	sql := BuildDeleteSQL("SELECT * FROM t WHERE name = 'foo'")
	if !strings.Contains(sql, "name = 'foo'") {
		t.Errorf("query text quotes altered inside dollar-quote: %s", sql)
	}
	if !strings.Contains(sql, "$sageqh$") {
		t.Errorf("expected $sageqh$ dollar tag: %s", sql)
	}
}

// TestBuildInsertSQL_TagCollision verifies chooseDollarTag picks a
// non-colliding tag when payload contains $sageqh$.
func TestBuildInsertSQL_TagCollision(t *testing.T) {
	// Query literally contains the default tag — builder must
	// escape to an alternate tag so the literal stays unambiguous.
	q := "SELECT '$sageqh$' FROM dual"
	sql := BuildInsertSQL(q, "hint")
	if !strings.Contains(sql, q) {
		t.Errorf("query text missing from output: %s", sql)
	}
	// Result must NOT end the string prematurely.
	if strings.Contains(sql, "$sageqh$"+q+"$sageqh$") {
		t.Errorf("collision: tag was not rotated: %s", sql)
	}
}

func TestBuildInsertSQL_OnConflict(t *testing.T) {
	sql := BuildInsertSQL("SELECT 1", "hint")
	if !strings.Contains(sql, "ON CONFLICT") {
		t.Errorf("should contain ON CONFLICT: %s", sql)
	}
}

func TestBuildDeleteSQL(t *testing.T) {
	sql := BuildDeleteSQL("SELECT * FROM foo")
	if sql == "" {
		t.Fatal("BuildDeleteSQL returned empty string")
	}
	if !strings.Contains(sql, "SELECT * FROM foo") {
		t.Errorf("should contain query text")
	}
	if !strings.Contains(sql, "DELETE") {
		t.Errorf("should contain DELETE: %s", sql)
	}
}
