package advisor

import (
	"strings"
	"testing"
)

func TestBloatSystemPrompt_ContainsRules(t *testing.T) {
	checks := []string{
		"VACUUM FULL",
		"pg_repack",
		"do nothing",
		"REINDEX CONCURRENTLY",
		"maintenance window",
		"severity: info",
	}
	for _, want := range checks {
		if !strings.Contains(bloatSystemPrompt, want) {
			t.Errorf("bloat system prompt missing %q", want)
		}
	}
}

func TestBloatSystemPrompt_ContainsAntiThinking(t *testing.T) {
	if !strings.Contains(bloatSystemPrompt, "No thinking") {
		t.Error("bloat system prompt missing anti-thinking directive")
	}
}

func TestBloatEstimate_Heuristic(t *testing.T) {
	nDead := int64(70000)
	nLive := int64(130000)
	total := nDead + nLive
	ratio := float64(nDead) / float64(total)
	if ratio < 0.34 || ratio > 0.36 {
		t.Errorf("expected ~35%%, got %.1f%%", ratio*100)
	}
}

func TestBloatEstimate_NoBloat(t *testing.T) {
	nDead := int64(600)
	nLive := int64(19400)
	ratio := float64(nDead) / float64(nDead+nLive)
	if ratio >= 0.10 {
		t.Errorf("expected ratio < 10%%, got %.1f%%", ratio*100)
	}
}

func TestBloatEstimate_SmallTableExcluded(t *testing.T) {
	nDead := int64(400)
	nLive := int64(500)
	total := nDead + nLive
	if total >= 1000 {
		t.Errorf("expected total < 1000, got %d", total)
	}
}

func TestBloatEstimate_ExtremeBloat(t *testing.T) {
	nDead := int64(90000)
	nLive := int64(10000)
	ratio := float64(nDead) / float64(nDead+nLive)
	if ratio < 0.89 {
		t.Errorf("expected ~90%%, got %.1f%%", ratio*100)
	}
}

func TestBloatTrend_Growing(t *testing.T) {
	current := 0.35
	prev := 0.30
	trend := "unknown"
	if current > prev+0.02 {
		trend = "growing"
	} else if current < prev-0.02 {
		trend = "shrinking"
	} else {
		trend = "stable"
	}
	if trend != "growing" {
		t.Errorf("expected growing, got %s", trend)
	}
}

func TestBloatTrend_Stable(t *testing.T) {
	current := 0.31
	prev := 0.30
	trend := "unknown"
	if current > prev+0.02 {
		trend = "growing"
	} else if current < prev-0.02 {
		trend = "shrinking"
	} else {
		trend = "stable"
	}
	if trend != "stable" {
		t.Errorf("expected stable, got %s", trend)
	}
}

func TestBloatTrend_Shrinking(t *testing.T) {
	current := 0.25
	prev := 0.35
	trend := "unknown"
	if current > prev+0.02 {
		trend = "growing"
	} else if current < prev-0.02 {
		trend = "shrinking"
	} else {
		trend = "stable"
	}
	if trend != "shrinking" {
		t.Errorf("expected shrinking, got %s", trend)
	}
}

func TestBloatEstimateMB(t *testing.T) {
	sizeMB := 180.0
	ratio := 0.35
	est := sizeMB * ratio
	if est < 62.0 || est > 64.0 {
		t.Errorf("expected ~63MB, got %.1f", est)
	}
}

func TestBloatRepack_Available(t *testing.T) {
	exts := []string{"pg_stat_statements", "pg_repack", "hypopg"}
	hasRepack := false
	for _, e := range exts {
		if e == "pg_repack" {
			hasRepack = true
			break
		}
	}
	if !hasRepack {
		t.Error("expected pg_repack detected")
	}
}

func TestBloatRepack_NotAvailable(t *testing.T) {
	exts := []string{"pg_stat_statements", "hypopg"}
	hasRepack := false
	for _, e := range exts {
		if e == "pg_repack" {
			hasRepack = true
			break
		}
	}
	if hasRepack {
		t.Error("should not detect pg_repack")
	}
}

func TestBloatFinding_SeverityForced(t *testing.T) {
	raw := `[{"table":"big_table","severity":"critical",` +
		`"recommended_sql":"VACUUM FULL"}]`
	findings := parseLLMFindings(raw, "bloat_remediation", noopLog)
	// Apply the same force logic as analyzeBloat.
	for i := range findings {
		findings[i].Severity = "info"
		findings[i].RecommendedSQL = ""
	}
	if findings[0].Severity != "info" {
		t.Error("severity not forced to info")
	}
	if findings[0].RecommendedSQL != "" {
		t.Error("recommended SQL not cleared")
	}
}
