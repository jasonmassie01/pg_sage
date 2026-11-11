package advisor

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// advisors-08: in fleet mode one database owns its instance's settings.
// Non-owners skip the WAL and connection sub-advisors, keep their open
// instance findings open (advisory, naming the owner), and every
// ALTER SYSTEM or managed parameter change they would make is advisory.

func ownedBy(owner string, isOwner bool) InstanceOwner {
	return func() (string, bool) { return owner, isOwner }
}

func walAndBloatConfigAdvisor(t *testing.T, content string, open *fakeOpen) (*Advisor,
	func() []capturedPrompt) {
	t.Helper()
	mgr, capture := capturingManager(t, content)
	cfg := walOnlyConfig("")
	cfg.Advisor.BloatEnabled = true
	return lifecycleAdvisor(cfg, mgr, open), capture
}

func hasPrompt(prompts []capturedPrompt, word string) bool {
	for _, p := range prompts {
		if strings.Contains(p.system+p.user, word) {
			return true
		}
	}
	return false
}

// CHECK-21: a non-owner's ALTER SYSTEM finding has empty recommended_sql
// and detail.instance_owner set.
func TestGateInstanceSettings_NonOwnerDefersAlterSystem(t *testing.T) {
	in := openWAL("ALTER SYSTEM SET max_wal_size = '4GB'")
	in.RollbackSQL = "ALTER SYSTEM RESET max_wal_size"
	in.Severity = "warning"
	out := GateInstanceSettings([]analyzer.Finding{in}, "alpha", false)
	if len(out) != 1 {
		t.Fatalf("findings = %d, want 1 (deferred, not dropped)", len(out))
	}
	f := out[0]
	if f.RecommendedSQL != "" || f.RollbackSQL != "" || f.ActionRisk != "" {
		t.Fatalf("non-owner finding still executable: sql=%q rollback=%q risk=%q",
			f.RecommendedSQL, f.RollbackSQL, f.ActionRisk)
	}
	if got := f.Detail[DetailInstanceOwner]; got != "alpha" {
		t.Fatalf("detail.%s = %v, want alpha", DetailInstanceOwner, got)
	}
	if !strings.Contains(f.Recommendation, "instance settings are managed from database alpha") {
		t.Fatalf("recommendation does not name the owner: %q", f.Recommendation)
	}
	if f.Severity != "info" {
		t.Fatalf("severity = %q, want info for advisory guidance", f.Severity)
	}
	if in.RecommendedSQL == "" || in.Detail[DetailInstanceOwner] != nil {
		t.Fatalf("the input finding was mutated: %+v", in)
	}
}

func TestGateInstanceSettings_OwnerAndPerDatabaseUnchanged(t *testing.T) {
	in := []analyzer.Finding{openWAL("ALTER SYSTEM SET max_wal_size = '4GB'")}
	for _, tc := range []struct {
		name    string
		owner   string
		isOwner bool
	}{
		{"owner", "alpha", true},
		{"identity unknown: per database", "", false},
		{"identity unknown, owner flag set", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := GateInstanceSettings(in, tc.owner, tc.isOwner)
			if len(out) != 1 || out[0].RecommendedSQL != in[0].RecommendedSQL ||
				out[0].Detail[DetailInstanceOwner] != nil {
				t.Fatalf("finding changed: %+v", out)
			}
		})
	}
}

// Nil/empty: no findings in, none out.
func TestGateInstanceSettings_Empty(t *testing.T) {
	if out := GateInstanceSettings(nil, "alpha", false); len(out) != 0 {
		t.Fatalf("nil in: %v", out)
	}
}

// Must not change: database-scope advice stays per database.
func TestGateInstanceSettings_DatabaseScopeKept(t *testing.T) {
	in := []analyzer.Finding{
		{Category: "wal_tuning", ObjectIdentifier: "instance:work_mem",
			RecommendedSQL: `ALTER DATABASE "app" SET work_mem = '64MB'`},
		{Category: "bloat_remediation", ObjectIdentifier: "public.orders",
			RecommendedSQL: "VACUUM (ANALYZE) public.orders"},
		{Category: "query_rewrite", ObjectIdentifier: "queryid:42",
			Recommendation: "rewrite the IN list as a join"},
	}
	out := GateInstanceSettings(in, "alpha", false)
	for i := range in {
		if out[i].RecommendedSQL != in[i].RecommendedSQL ||
			out[i].Detail[DetailInstanceOwner] != nil {
			t.Fatalf("database-scope finding %d changed: %+v", i, out[i])
		}
	}
}

// A multi-statement or oddly spaced ALTER SYSTEM is still instance scope:
// the gate fails closed on any ALTER SYSTEM statement.
func TestGateInstanceSettings_AnyAlterSystemIsInstanceScope(t *testing.T) {
	for _, sql := range []string{
		"ALTER SYSTEM SET max_wal_size = '8GB'; ALTER SYSTEM SET checkpoint_timeout = '15min'",
		"alter   system set max_connections = 200",
		"ALTER\nSYSTEM RESET max_wal_size",
		"SELECT 1; ALTER SYSTEM SET wal_compression = on",
	} {
		out := GateInstanceSettings([]analyzer.Finding{openWAL(sql)}, "alpha", false)
		if out[0].RecommendedSQL != "" || out[0].Detail[DetailInstanceOwner] != "alpha" {
			t.Fatalf("%q not deferred: %+v", sql, out[0])
		}
	}
}

// Managed intents (RDS parameter group, Cloud SQL flag) are created only by
// the owner: a non-owner's intent is stripped.
func TestGateInstanceSettings_NonOwnerStripsManagedIntent(t *testing.T) {
	f := withManagedIntent(openWAL(""), "rds", "ALTER SYSTEM SET max_wal_size = '4GB'",
		"forced checkpoints")
	if _, ok := managedparam.IntentFromDetail(f.Detail); !ok {
		t.Fatalf("precondition: intent not attached: %v", f.Detail)
	}
	out := GateInstanceSettings([]analyzer.Finding{f}, "alpha", false)
	if _, ok := managedparam.IntentFromDetail(out[0].Detail); ok {
		t.Fatalf("non-owner kept the managed intent: %v", out[0].Detail)
	}
	if _, ok := out[0].Detail[analyzer.DetailApprovalRequired]; ok {
		t.Fatalf("non-owner kept the approval note: %v", out[0].Detail)
	}
	if out[0].Detail[DetailInstanceOwner] != "alpha" {
		t.Fatalf("detail = %v, want instance_owner alpha", out[0].Detail)
	}
	owner := GateInstanceSettings([]analyzer.Finding{f}, "alpha", true)
	if _, ok := managedparam.IntentFromDetail(owner[0].Detail); !ok {
		t.Fatalf("the owner lost its managed intent: %v", owner[0].Detail)
	}
}

// Happy path, owner: the WAL sub-advisor runs and wal_tuning is evaluated.
func TestInstanceOwner_OwnerRunsWAL(t *testing.T) {
	a, capture := walAndBloatConfigAdvisor(t, walAdvice, &fakeOpen{})
	a.WithInstanceOwner(ownedBy("alpha", true))
	got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if !hasPrompt(capture(), "max_wal_size") {
		t.Fatal("owner sent no WAL prompt")
	}
	if len(got) != 1 || got[0].RecommendedSQL == "" || got[0].Detail[DetailInstanceOwner] != nil {
		t.Fatalf("owner findings = %+v, want one executable WAL finding", got)
	}
	if !slices.Contains(a.LastEvaluatedCategories(), "wal_tuning") {
		t.Fatalf("evaluated = %v, want wal_tuning", a.LastEvaluatedCategories())
	}
}

// A non-owner makes no WAL or connection model call, does not report
// those categories evaluated, and still runs its database-scope advisors.
func TestInstanceOwner_NonOwnerSkipsInstanceSubAdvisors(t *testing.T) {
	a, capture := walAndBloatConfigAdvisor(t, "[]", &fakeOpen{})
	a.cfg.Advisor.ConnectionEnabled = true
	a.WithInstanceOwner(ownedBy("alpha", false))
	snap := walSnapAt("1024")
	snap.Tables = deadTupleSnapshot("orders").Tables
	a.analyzeSnapshot(context.Background(), snap)
	prompts := capture()
	if len(prompts) != 1 {
		t.Fatalf("model calls = %d, want 1 (bloat only)", len(prompts))
	}
	if hasPrompt(prompts, "max_wal_size") || hasPrompt(prompts, "max_connections") {
		t.Fatalf("non-owner asked an instance question: %+v", prompts)
	}
	ev := a.LastEvaluatedCategories()
	if slices.Contains(ev, "wal_tuning") || slices.Contains(ev, "connection_tuning") {
		t.Fatalf("evaluated = %v: a non-owner would resolve instance rows", ev)
	}
	if !slices.Contains(ev, "bloat_remediation") {
		t.Fatalf("evaluated = %v, want bloat_remediation (database scope)", ev)
	}
}

// A non-owner's open instance finding stays open: re-emitted with
// detail.instance_owner and no executable SQL, even when its setting is
// live (only the owner resolves it).
func TestInstanceOwner_NonOwnerKeepsOpenInstanceFinding(t *testing.T) {
	open := &fakeOpen{findings: []analyzer.Finding{
		openWAL("ALTER SYSTEM SET max_wal_size = '4GB'")}}
	a, capture := walAndBloatConfigAdvisor(t, "[]", open)
	a.cfg.Advisor.BloatEnabled = false
	a.WithInstanceOwner(ownedBy("alpha", false))
	got := a.analyzeSnapshot(context.Background(), walSnapAt("4096"))
	if len(capture()) != 0 {
		t.Fatalf("model calls = %d, want 0", len(capture()))
	}
	if len(got) != 1 || got[0].Category != "wal_tuning" ||
		got[0].ObjectIdentifier != "instance" {
		t.Fatalf("findings = %v, want the open wal_tuning|instance kept", findingIdents(got))
	}
	if got[0].RecommendedSQL != "" || got[0].Detail[DetailInstanceOwner] != "alpha" {
		t.Fatalf("kept finding = %+v, want advisory naming alpha", got[0])
	}
}

// State transition: a database that takes ownership keeps the advisory
// rows it held as a non-owner open until its own review of the category
// runs; that review's fresh finding replaces the row instead of being
// dropped by the open gate, and a review that advises nothing for it
// resolves the row (it no longer names a stale owner forever).
func TestInstanceOwner_NewOwnerReclaimsDeferredFinding(t *testing.T) {
	deferred := deferredWAL()
	t.Run("review not due yet: row stays open", func(t *testing.T) {
		open := &fakeOpen{}
		a, capture := walAndBloatConfigAdvisor(t, "[]", open)
		a.cfg.Advisor.BloatEnabled = false
		a.WithInstanceOwner(ownedBy("beta", true))
		a.analyzeSnapshot(context.Background(), walSnapAt("1024")) // claims the run
		open.findings = []analyzer.Finding{deferred}
		got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
		if len(capture()) != 1 {
			t.Fatalf("model calls = %d, want 1 (the second cycle is not due)", len(capture()))
		}
		if len(got) != 1 || got[0].ObjectIdentifier != "instance" ||
			got[0].Detail[DetailInstanceOwner] != "alpha" {
			t.Fatalf("findings = %+v, want the deferred row kept open", got)
		}
	})
	t.Run("model silent: row resolves", func(t *testing.T) {
		a, capture := walAndBloatConfigAdvisor(t, "[]", &fakeOpen{
			findings: []analyzer.Finding{deferred}})
		a.cfg.Advisor.BloatEnabled = false
		a.WithInstanceOwner(ownedBy("beta", true))
		got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
		if !hasPrompt(capture(), "max_wal_size = 1024") {
			t.Fatal("the new owner's prompt left out the deferred setting")
		}
		if len(got) != 0 {
			t.Fatalf("findings = %v, want the deferred row resolved", findingIdents(got))
		}
		if !slices.Contains(a.LastEvaluatedCategories(), "wal_tuning") {
			t.Fatalf("evaluated = %v, want wal_tuning so the row resolves",
				a.LastEvaluatedCategories())
		}
	})
	t.Run("model re-advises: fresh finding replaces it", func(t *testing.T) {
		a, capture := walAndBloatConfigAdvisor(t, walAdvice, &fakeOpen{
			findings: []analyzer.Finding{deferred}})
		a.cfg.Advisor.BloatEnabled = false
		a.WithInstanceOwner(ownedBy("beta", true))
		got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
		if !hasPrompt(capture(), "max_wal_size = 1024") {
			t.Fatal("the new owner's prompt left out the deferred setting")
		}
		if len(got) != 1 || got[0].RecommendedSQL == "" ||
			got[0].Detail[DetailInstanceOwner] != nil {
			t.Fatalf("findings = %+v, want one fresh executable finding", got)
		}
	})
}

// deferredWAL is an open max_wal_size row a non-owner deferred to alpha.
func deferredWAL() analyzer.Finding {
	return GateInstanceSettings([]analyzer.Finding{
		openWAL("ALTER SYSTEM SET max_wal_size = '4GB'")}, "alpha", false)[0]
}

// State transition: a new owner whose review fails keeps the deferred row
// open and leaves wal_tuning unevaluated, so nothing resolves it.
func TestInstanceOwner_NewOwnerReviewFailsKeepsDeferred(t *testing.T) {
	deferred := deferredWAL()
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{
		findings: []analyzer.Finding{deferred}})
	a.WithInstanceOwner(ownedBy("beta", true))
	got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if len(got) != 1 || got[0].Detail[DetailInstanceOwner] != "alpha" {
		t.Fatalf("findings = %+v, want the deferred row kept open", got)
	}
	if slices.Contains(a.LastEvaluatedCategories(), "wal_tuning") {
		t.Fatalf("evaluated = %v, want wal_tuning unevaluated after a failure",
			a.LastEvaluatedCategories())
	}
}

// Nil owner function: single-database behaviour, unchanged.
func TestInstanceOwner_NilOwnerIsPerDatabase(t *testing.T) {
	a, capture := walAndBloatConfigAdvisor(t, walAdvice, &fakeOpen{})
	a.cfg.Advisor.BloatEnabled = false
	a.WithInstanceOwner(nil)
	got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if len(capture()) != 1 || len(got) != 1 || got[0].RecommendedSQL == "" {
		t.Fatalf("calls=%d findings=%+v, want the per-database review", len(capture()), got)
	}
}

// Empty identity (owner "" from the registry): per-database behaviour.
func TestInstanceOwner_EmptyOwnerIsPerDatabase(t *testing.T) {
	a, capture := walAndBloatConfigAdvisor(t, walAdvice, &fakeOpen{})
	a.cfg.Advisor.BloatEnabled = false
	a.WithInstanceOwner(ownedBy("", false))
	got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if len(capture()) != 1 || len(got) != 1 || got[0].RecommendedSQL == "" {
		t.Fatalf("calls=%d findings=%+v, want the per-database review", len(capture()), got)
	}
}

// State transition: ownership is read every cycle, so a database that
// loses it stops asking on the next cycle and one that gains it starts.
func TestInstanceOwner_OwnershipReadEachCycle(t *testing.T) {
	a, capture := walAndBloatConfigAdvisor(t, "[]", &fakeOpen{})
	a.cfg.Advisor.BloatEnabled = false
	a.cfg.Advisor.IntervalSeconds = 1
	isOwner := false
	a.WithInstanceOwner(func() (string, bool) { return "alpha", isOwner })
	a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if len(capture()) != 0 {
		t.Fatalf("non-owner calls = %d", len(capture()))
	}
	isOwner = true
	a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if len(capture()) != 1 {
		t.Fatalf("after taking ownership calls = %d, want 1", len(capture()))
	}
}
