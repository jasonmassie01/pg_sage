package analyzer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/collector"
)

// Without a consumption rate an XID or sequence finding shows its
// headroom and stays unassessed (cases-07): no time to impact is derived
// from an age or a percentage alone.

func asCase(t *testing.T, f Finding) cases.Case {
	t.Helper()
	raw, err := json.Marshal(f.Detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	return cases.ProjectFinding(cases.SourceFinding{Category: f.Category,
		Severity: cases.Severity(f.Severity), Detail: detail}, nil)
}

func TestUrgencyContract_XIDWraparoundShowsHeadroom(t *testing.T) {
	cfg := coverageTestConfig()
	cfg.Analyzer.XIDWraparoundCritical = 1600000000
	fs := ruleXIDWraparound(1200000000, cfg)
	if len(fs) != 1 {
		t.Fatalf("ruleXIDWraparound = %+v", fs)
	}
	c := asCase(t, fs[0])
	if c.Urgency.Assessed || c.Urgency.TimeToImpactSeconds != nil {
		t.Fatalf("urgency = %+v, want unassessed", c.Urgency)
	}
	if c.Urgency.Basis != "xid_age 1.2B of the 1.6B critical threshold" {
		t.Fatalf("basis = %q", c.Urgency.Basis)
	}
	if !strings.HasPrefix(c.WhyNow, "xid_age 1.2B of the 1.6B critical threshold;") ||
		strings.Contains(c.WhyNow, "Urgency not assessed") {
		t.Fatalf("why now = %q", c.WhyNow)
	}
}

func TestUrgencyContract_SequenceExhaustionShowsHeadroom(t *testing.T) {
	snap := &collector.Snapshot{Sequences: []collector.SequenceStats{{
		SchemaName: "public", SequenceName: "orders_id_seq", PctUsed: 92.34,
		DataType: "integer", IncrementBy: 1,
	}}}
	fs := ruleSequenceExhaustion(snap, nil, nil, nil)
	if len(fs) != 1 {
		t.Fatalf("ruleSequenceExhaustion = %+v", fs)
	}
	c := asCase(t, fs[0])
	if c.Urgency.Assessed || c.Urgency.TimeToImpactSeconds != nil {
		t.Fatalf("urgency = %+v, want unassessed", c.Urgency)
	}
	if c.Urgency.Basis != "usage_pct 92.3 of the sequence range" {
		t.Fatalf("basis = %q", c.Urgency.Basis)
	}
	if !strings.Contains(c.WhyNow,
		"time to exhaustion and application impact have not been established") {
		t.Fatalf("why now = %q", c.WhyNow)
	}
}
