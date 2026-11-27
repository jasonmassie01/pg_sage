package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// vacuum-08: the analyzer diagnoses wraparound against each table's
// effective freeze maximum (the shared horizon reader) and never proposes
// the VACUUM: the freeze custodian is its only producer. No concurrency
// test: the rule is a pure function of its arguments.

func horizon(schema, table string, age, maxAge int64) probes.FreezeHorizon {
	return probes.FreezeHorizon{Schema: schema, Table: table, XIDAge: age,
		XIDMaxAge: maxAge, MultiXactAge: 1, MultiXactMaxAge: 400_000_000}
}

func freezeExtras(h ...probes.FreezeHorizon) *RuleExtras {
	return &RuleExtras{FreezeHorizons: h}
}

func anySnapshot() *collector.Snapshot {
	return &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "t"}}}
}

func TestRuleWraparoundFreeze_ShareBoundary(t *testing.T) {
	cfg := testConfig()
	below := ruleWraparoundFreeze(anySnapshot(), nil, cfg,
		freezeExtras(horizon("public", "below", 7_499, 10_000)))
	if len(below) != 0 {
		t.Fatalf("share 0.7499 gave %#v, want no finding", below)
	}
	at := ruleWraparoundFreeze(anySnapshot(), nil, cfg,
		freezeExtras(horizon("public", "at", 7_500, 10_000)))
	if len(at) != 1 || at[0].Severity != "warning" || at[0].ObjectIdentifier != "public.at" ||
		at[0].Category != "wraparound_freeze" {
		t.Fatalf("share 0.75 gave %#v, want one warning for public.at", at)
	}
}

// The finding is diagnosis only: no statement and no action risk, and its
// detail points at the custodian's decision.
func TestRuleWraparoundFreeze_DiagnosisOnly(t *testing.T) {
	f := ruleWraparoundFreeze(anySnapshot(), nil, testConfig(),
		freezeExtras(horizon("public", "hot", 210_000_000, 200_000_000)))
	if len(f) != 1 {
		t.Fatalf("findings = %#v, want one", f)
	}
	got := f[0]
	if got.RecommendedSQL != "" || got.ActionRisk != "" || got.RollbackSQL != "" {
		t.Fatalf("finding proposes an action: sql %q risk %q", got.RecommendedSQL,
			got.ActionRisk)
	}
	if got.Severity != "critical" {
		t.Fatalf("past its maximum: severity %q, want critical", got.Severity)
	}
	link, ok := got.Detail["custodian_decision"].(map[string]any)
	if !ok || link["feature"] != "freeze" || link["target"] != "public.hot" {
		t.Fatalf("detail does not link the custodian decision: %#v", got.Detail)
	}
	if got.Detail["freeze_max_age"] != int64(200_000_000) || got.Detail["xid_age"] !=
		int64(210_000_000) || !strings.Contains(got.Recommendation, "custodian") {
		t.Fatalf("detail/recommendation = %#v / %q", got.Detail, got.Recommendation)
	}
}

// A DBA's autovacuum_freeze_max_age of 1e9 is respected: 300M is green.
func TestRuleWraparoundFreeze_RespectsTheTunedMaximum(t *testing.T) {
	f := ruleWraparoundFreeze(anySnapshot(), nil, testConfig(),
		freezeExtras(horizon("public", "big", 300_000_000, 1_000_000_000),
			horizon("public", "tuned", 80_000, 100_000)))
	if len(f) != 1 || f[0].ObjectIdentifier != "public.tuned" {
		t.Fatalf("findings = %#v, want public.tuned only", f)
	}
}

func TestRuleWraparoundFreeze_MultiXactAndCatalogs(t *testing.T) {
	mx := horizon("public", "mx", 1, 200_000_000)
	mx.MultiXactAge, mx.MultiXactMaxAge = 350_000_000, 400_000_000
	cat := horizon("pg_catalog", "pg_class", 160_000_000, 200_000_000)
	cat.Catalog = true
	own := horizon("sage", "findings", 199_000_000, 200_000_000)
	f := ruleWraparoundFreeze(anySnapshot(), nil, testConfig(), freezeExtras(mx, cat, own))
	byObject := map[string]Finding{}
	for _, x := range f {
		byObject[x.ObjectIdentifier] = x
	}
	if len(f) != 2 || byObject["public.mx"].Category == "" ||
		byObject["pg_catalog.pg_class"].Detail["catalog"] != true {
		t.Fatalf("findings = %#v, want the multixact table and the catalog", f)
	}
	if _, flagged := byObject["sage.findings"]; flagged {
		t.Fatal("pg_sage's own schema was diagnosed")
	}
}

func TestRuleWraparoundFreeze_NilEmptyAndInvalidShare(t *testing.T) {
	cfg := testConfig()
	if got := ruleWraparoundFreeze(anySnapshot(), nil, cfg, nil); got != nil {
		t.Fatalf("nil extras gave %#v", got)
	}
	if got := ruleWraparoundFreeze(anySnapshot(), nil, cfg, &RuleExtras{}); got != nil {
		t.Fatalf("no horizons gave %#v", got)
	}
	unknown := horizon("public", "unknown", 5, 0)
	unknown.MultiXactMaxAge = 0
	if got := ruleWraparoundFreeze(nil, nil, cfg, freezeExtras(unknown)); len(got) != 0 {
		t.Fatalf("unknown maxima gave %#v", got)
	}
	cfg.Analyzer.WraparoundFreezeShare = 0 // unset: the shipped 0.75
	if got := ruleWraparoundFreeze(nil, nil, cfg,
		freezeExtras(horizon("public", "x", 7_400, 10_000))); len(got) != 0 {
		t.Fatalf("zero share fell below the default: %#v", got)
	}
	cfg.Analyzer.WraparoundFreezeShare = 0.5
	if got := ruleWraparoundFreeze(nil, nil, cfg,
		freezeExtras(horizon("public", "x", 5_000, 10_000))); len(got) != 1 {
		t.Fatalf("configured share 0.5 ignored: %#v", got)
	}
}
