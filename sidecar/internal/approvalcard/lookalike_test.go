package approvalcard

import (
	"strings"
	"testing"
)

// Fleet learning: a card shows look-alike evidence labelled as coming from
// other databases, next to (never instead of) the local calibration.

func TestCardShowsLookalikePrior(t *testing.T) {
	c := agentCard(map[string]any{"llm_rationale": "seq scans on customer_id",
		"producer": "tuning_agent",
		"confidence_calibration": map[string]any{"status": "uncalibrated", "n": float64(1),
			"min_outcomes": float64(5)},
		"lookalike_prior": map[string]any{"source": "from look-alike databases",
			"databases": float64(3), "improved": float64(9), "outcomes": float64(10),
			"regressed": float64(0), "match": "table_shape"}})
	r := c.Rationale
	if r == nil || r.LookAlike != "9 of 10 comparable actions improved on 3 "+
		"look-alike databases (same table shape)" {
		t.Fatalf("rationale = %+v", r)
	}
	if r.Confidence != nil {
		t.Fatal("a prior must not become the card's confidence")
	}
	text := Text(c, now)
	if !strings.Contains(text, "From look-alike databases: 9 of 10") {
		t.Fatalf("text:\n%s", text)
	}
	if !strings.Contains(text, "uncalibrated (1 of 5 outcomes)") {
		t.Fatal("the local calibration must still be shown")
	}
}

func TestCardLookalikeClassMatchAndRegressions(t *testing.T) {
	got := lookalikeText(map[string]any{"databases": float64(1), "improved": float64(1),
		"outcomes": float64(5), "regressed": float64(4), "match": "action_class"})
	want := "1 of 5 comparable actions improved, 4 regressed, on 1 look-alike " +
		"database (same kind of action)"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCardLookalikeMalformed(t *testing.T) {
	for _, raw := range []any{nil, "x", map[string]any{},
		map[string]any{"outcomes": float64(0), "databases": float64(2)},
		map[string]any{"outcomes": "ten"}} {
		if got := lookalikeText(raw); got != "" {
			t.Fatalf("lookalikeText(%v) = %q, want empty", raw, got)
		}
	}
	c := agentCard(map[string]any{"llm_rationale": "x"})
	if c.Rationale.LookAlike != "" || strings.Contains(Text(c, now), "look-alike") {
		t.Fatal("no prior, no look-alike line")
	}
}
