package analyzer

import (
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestRuleWraparoundFreeze_WarningAndCritical(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 150_000_000
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "warm", XIDAge: 160_000_000},
		{SchemaName: "public", RelName: "hot", XIDAge: 210_000_000},
		{SchemaName: "public", RelName: "fine", XIDAge: 1_000_000},
	}}
	f := ruleWraparoundFreeze(snap, nil, cfg, nil)
	if len(f) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(f))
	}
	bySeverity := map[string]Finding{}
	for _, x := range f {
		bySeverity[x.Severity] = x
	}
	if _, ok := bySeverity["warning"]; !ok {
		t.Error("expected a warning finding")
	}
	crit, ok := bySeverity["critical"]
	if !ok {
		t.Fatal("expected a critical finding for age >= 200M")
	}
	if !strings.Contains(crit.RecommendedSQL, `VACUUM (FREEZE) "public"."hot"`) {
		t.Errorf("RecommendedSQL = %q", crit.RecommendedSQL)
	}
	if crit.ActionRisk != "safe" {
		t.Errorf("ActionRisk = %q", crit.ActionRisk)
	}
}

func TestRuleWraparoundFreeze_NilAndBelowThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 150_000_000
	if got := ruleWraparoundFreeze(nil, nil, cfg, nil); got != nil {
		t.Errorf("expected nil for nil snapshot")
	}
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "young", XIDAge: 100},
	}}
	if got := ruleWraparoundFreeze(snap, nil, cfg, nil); len(got) != 0 {
		t.Errorf("expected 0 findings below threshold, got %d", len(got))
	}
}

// vacuum-01: the warning starts at the configured threshold and the
// critical at 200M, both inclusive.
func TestRuleWraparoundFreeze_SeverityBoundaries(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 150_000_000
	for _, tc := range []struct {
		age  int64
		want string
	}{
		{149_999_999, ""}, {150_000_000, "warning"},
		{199_999_999, "warning"}, {200_000_000, "critical"},
	} {
		snap := &collector.Snapshot{Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "t", Relkind: "r", XIDAge: tc.age},
		}}
		got := ruleWraparoundFreeze(snap, nil, cfg, nil)
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("age %d: %d findings, want none", tc.age, len(got))
		case tc.want != "" && (len(got) != 1 || got[0].Severity != tc.want):
			t.Errorf("age %d: %+v, want one %s", tc.age, got, tc.want)
		}
	}
}

// vacuum-01: a partitioned parent has no storage of its own (relfrozenxid
// 0), and a VACUUM of it recurses into every partition. It is never a
// finding, whatever age it carries; its leaves are assessed individually.
func TestRuleWraparoundFreeze_PartitionedParentNeverFlagged(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 150_000_000
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "parent", Relkind: "p", XIDAge: math.MaxInt32},
		{SchemaName: "public", RelName: "parent2", Relkind: "p", XIDAge: 300_000_000},
		{SchemaName: "public", RelName: "leaf", Relkind: "r", XIDAge: 210_000_000},
		{SchemaName: "public", RelName: "mv", Relkind: "m", XIDAge: 160_000_000},
	}}
	got := ruleWraparoundFreeze(snap, nil, cfg, nil)
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want the leaf and the matview only", got)
	}
	idents := map[string]string{}
	for _, f := range got {
		idents[f.ObjectIdentifier] = f.Severity
	}
	if idents["public.leaf"] != "critical" || idents["public.mv"] != "warning" {
		t.Fatalf("findings by object = %v", idents)
	}
}

// vacuum-01: an unknown age (-1) is never a finding, and neither is the
// non-normal-XID sentinel 2147483647 from a row without a relkind (a
// snapshot collected before relkind existed): no real table reaches it,
// since PostgreSQL stops assigning XIDs well before.
func TestRuleWraparoundFreeze_UnknownAgeIgnored(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 1
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "unknown", Relkind: "r", XIDAge: -1},
		{SchemaName: "public", RelName: "legacy_parent", XIDAge: math.MaxInt32},
		{SchemaName: "public", RelName: "negative", Relkind: "r", XIDAge: -42},
	}}
	if got := ruleWraparoundFreeze(snap, nil, cfg, nil); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
}

func TestRuleWraparoundFreeze_EmptyTablesGiveNil(t *testing.T) {
	cfg := testConfig()
	for _, snap := range []*collector.Snapshot{
		{}, {Tables: []collector.TableStats{}},
	} {
		if got := ruleWraparoundFreeze(snap, nil, cfg, nil); got != nil {
			t.Fatalf("findings = %+v, want nil", got)
		}
	}
}

// vacuum-01: the finding carries the multixact age next to the XID age.
func TestRuleWraparoundFreeze_DetailCarriesMXIDAge(t *testing.T) {
	cfg := testConfig()
	cfg.Analyzer.WraparoundFreezeXIDAge = 150_000_000
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "hot", Relkind: "r", XIDAge: 210_000_000,
			MXIDAge: 12_345},
	}}
	got := ruleWraparoundFreeze(snap, nil, cfg, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	d := got[0].Detail
	if d["xid_age"] != int64(210_000_000) || d["mxid_age"] != int64(12_345) ||
		d["threshold"] != int64(150_000_000) || d["critical_age"] != criticalFreezeXIDAge {
		t.Fatalf("detail = %v", d)
	}
}

// No concurrent access test: ruleWraparoundFreeze is a pure function of its
// snapshot and shares no state.
