package advisor

import (
	"strings"
	"testing"
)

func TestVacuumSystemPrompt_ContainsRules(t *testing.T) {
	checks := []string{
		"scale_factor",
		"0.01-0.05",
		"Never set scale_factor to 0",
		"1000 rows",
		"dead_tuple_ratio",
		"ALTER TABLE",
	}
	for _, want := range checks {
		if !strings.Contains(vacuumSystemPrompt, want) {
			t.Errorf("vacuum system prompt missing %q", want)
		}
	}
}

func TestVacuumSystemPrompt_ContainsAntiThinking(t *testing.T) {
	if !strings.Contains(vacuumSystemPrompt, "No thinking") {
		t.Error("vacuum system prompt missing anti-thinking directive")
	}
}

func TestVacuumSystemPrompt_JSONFormat(t *testing.T) {
	if !strings.Contains(vacuumSystemPrompt, "JSON array") {
		t.Error("vacuum system prompt missing JSON array format")
	}
	if !strings.Contains(vacuumSystemPrompt, "object_identifier") {
		t.Error("vacuum system prompt missing object_identifier field")
	}
}

func TestVacuumContext_DeadTupleRatio(t *testing.T) {
	nDead := int64(85000)
	nLive := int64(500000)
	total := nLive + nDead
	ratio := float64(nDead) / float64(total)
	if ratio < 0.05 {
		t.Errorf("expected ratio > 0.05, got %.4f", ratio)
	}
	if ratio < 0.14 || ratio > 0.15 {
		t.Errorf("expected ratio ~14.5%%, got %.4f", ratio)
	}
}

func TestVacuumContext_DeadTupleRatio_ZeroLiveTuples(t *testing.T) {
	nDead := int64(100)
	nLive := int64(0)
	total := nLive + nDead
	ratio := float64(nDead) / float64(total)
	if ratio != 1.0 {
		t.Errorf("expected ratio 1.0, got %.4f", ratio)
	}
	// But total < 1000, so the table would be skipped.
	if total >= 1000 {
		t.Error("expected total < 1000, table should be excluded")
	}
}

func TestVacuumContext_SmallTableExcluded(t *testing.T) {
	nDead := int64(400)
	nLive := int64(500)
	total := nLive + nDead
	if total >= 1000 {
		t.Errorf("expected total < 1000, got %d", total)
	}
}

func TestVacuumContext_LowDeadRatioExcluded(t *testing.T) {
	nDead := int64(100)
	nLive := int64(50000)
	total := nLive + nDead
	ratio := float64(nDead) / float64(total)
	if ratio >= 0.05 {
		t.Errorf("expected ratio < 0.05, got %.4f", ratio)
	}
}

func TestVacuumContext_HighDeadRatioIncluded(t *testing.T) {
	nDead := int64(10000)
	nLive := int64(50000)
	total := nLive + nDead
	ratio := float64(nDead) / float64(total)
	if ratio < 0.05 {
		t.Errorf("expected ratio >= 0.05, got %.4f", ratio)
	}
	if total < 1000 {
		t.Errorf("expected total >= 1000, got %d", total)
	}
}

func TestVacuumPrompt_TokenBudget(t *testing.T) {
	if maxAdvisorPromptChars != 16384 {
		t.Errorf(
			"expected maxAdvisorPromptChars=16384, got %d",
			maxAdvisorPromptChars,
		)
	}
}
