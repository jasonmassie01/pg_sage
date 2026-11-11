package advisor

import (
	"context"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// advisors-02 review: an open single-statement WAL finding is keyed
// "instance". A new single-statement proposal for another setting gets
// the same key, so dropping by key alone silenced the whole sub-advisor
// again. The new proposal is kept under its own per-setting key instead,
// and the open row is still re-emitted unchanged.
func TestLifecycle_SingleStatementOtherGUCNotSilenced(t *testing.T) {
	mgr, _ := capturingManager(t, `[{"object_identifier":"instance","rationale":"r",`+
		`"recommended_sql":"ALTER SYSTEM SET checkpoint_timeout = '15min';"}]`)
	f := openWAL("ALTER SYSTEM SET max_wal_size = '4GB';")
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{findings: []analyzer.Finding{f}})
	got := a.analyzeSnapshot(context.Background(), walSnapAt("2048"))
	want := []string{"wal_tuning|instance", "wal_tuning|instance:checkpoint_timeout"}
	if !slices.Equal(findingIdents(got), want) {
		t.Fatalf("findings = %v, want %v", findingIdents(got), want)
	}
	if got[0].RecommendedSQL != f.RecommendedSQL {
		t.Fatalf("open row overwritten: %q", got[0].RecommendedSQL)
	}
	if got[1].Title != "wal_tuning: set checkpoint_timeout" ||
		got[1].RecommendedSQL != "ALTER SYSTEM SET checkpoint_timeout = '15min';" {
		t.Fatalf("re-keyed finding = %+v", got[1])
	}
}

// A single-statement proposal for the open setting itself is still
// dropped, whatever its key.
func TestLifecycle_SingleStatementSameGUCDropped(t *testing.T) {
	mgr, _ := capturingManager(t, `[{"object_identifier":"instance","rationale":"r",`+
		`"recommended_sql":"ALTER SYSTEM SET max_wal_size = '8GB';"}]`)
	f := openWAL("ALTER SYSTEM SET max_wal_size = '4GB';")
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{findings: []analyzer.Finding{f}})
	got := a.analyzeSnapshot(context.Background(), walSnapAt("2048"))
	if len(got) != 1 || got[0].RecommendedSQL != f.RecommendedSQL {
		t.Fatalf("findings = %+v, want only the open max_wal_size row", got)
	}
}

// rekey leaves alone a finding whose key is free, one with no setting to
// key by, and any category that is not a setting category.
func TestOpenGateRekey(t *testing.T) {
	g := newOpenGate([]analyzer.Finding{
		{Category: "wal_tuning", ObjectIdentifier: "instance",
			RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB'"},
		{Category: "bloat_remediation", ObjectIdentifier: "public.t1"},
	})
	free := analyzer.Finding{Category: "connection_tuning", ObjectIdentifier: "instance",
		RecommendedSQL: "ALTER SYSTEM SET max_connections = 200"}
	if got := g.rekey("connection_tuning", free); got.ObjectIdentifier != "instance" {
		t.Errorf("free key re-keyed to %q", got.ObjectIdentifier)
	}
	advisory := analyzer.Finding{Category: "wal_tuning", ObjectIdentifier: "instance"}
	if got := g.rekey("wal_tuning", advisory); got.ObjectIdentifier != "instance" {
		t.Errorf("finding without a setting re-keyed to %q", got.ObjectIdentifier)
	}
	bloat := analyzer.Finding{Category: "bloat_remediation", ObjectIdentifier: "public.t1"}
	if got := g.rekey("bloat_remediation", bloat); got.ObjectIdentifier != "public.t1" {
		t.Errorf("bloat finding re-keyed to %q", got.ObjectIdentifier)
	}
	if out := g.dropOpen("bloat_remediation", []analyzer.Finding{bloat}, noopLog); len(out) != 0 {
		t.Errorf("open bloat table not dropped: %v", out)
	}
}

// A re-emitted open finding is never rated below the moderate risk the
// advisor gives every setting change, whatever form its SQL now has; an
// advisory row without SQL carries no risk.
func TestReemitRisk(t *testing.T) {
	cases := map[string]string{
		"":                                       "",
		"ALTER SYSTEM SET max_wal_size = '4GB';": "moderate",
		"ALTER DATABASE app SET work_mem = '64MB'": "moderate",
	}
	for sql, want := range cases {
		if got := reemitRisk(sql); got != want {
			t.Errorf("reemitRisk(%q) = %q, want %q", sql, got, want)
		}
	}
}
