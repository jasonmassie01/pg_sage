package executor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// executor-01: the soft-drop restore of an autonomous index drop. Each
// drop rule ships pg_get_indexdef() + ";" as its rollback; the restore must
// be the CONCURRENTLY form of exactly that definition and run under its own
// typed contract, not be parked by the gate.

// dropRuleRollbacks are the rollbacks the four drop rules ship
// (rules_unused_index.go, rules_index.go invalid/duplicate/subset): the
// definition pg_get_indexdef() prints, plus ";", with the quoted DROP the
// rules run (dropIndexSQL).
var dropRuleRollbacks = []struct {
	rule, drop, rollback, want string
}{
	{"unused_index", `DROP INDEX CONCURRENTLY "public"."orders_status_idx";`,
		"CREATE INDEX orders_status_idx ON public.orders USING btree (status);",
		"CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders USING btree (status)"},
	{"invalid_index", `DROP INDEX CONCURRENTLY "app"."events_kind_idx";`,
		"CREATE INDEX events_kind_idx ON app.events USING btree (kind) WHERE (kind <> 0);",
		"CREATE INDEX CONCURRENTLY events_kind_idx ON app.events USING btree (kind) " +
			"WHERE (kind <> 0)"},
	{"duplicate_index", `DROP INDEX CONCURRENTLY "public"."users_email_key2";`,
		"CREATE UNIQUE INDEX users_email_key2 ON public.users USING btree (email);",
		"CREATE UNIQUE INDEX CONCURRENTLY users_email_key2 ON public.users USING btree " +
			"(email)"},
	{"subset_index", `DROP INDEX CONCURRENTLY "public"."Orders_A_idx";`,
		`CREATE INDEX "Orders_A_idx" ON public.orders USING btree (a) INCLUDE (b);`,
		`CREATE INDEX CONCURRENTLY "Orders_A_idx" ON public.orders USING btree (a) ` +
			`INCLUDE (b)`},
}

func TestRestoreSQLForDropIsTheConcurrentDefinition(t *testing.T) {
	for _, c := range dropRuleRollbacks {
		got := restoreSQLForDrop(c.rollback)
		if got != c.want {
			t.Fatalf("%s: restore = %q, want %q", c.rule, got, c.want)
		}
		if again := restoreSQLForDrop(got); again != got {
			t.Fatalf("%s: normalization is not idempotent: %q -> %q", c.rule, got, again)
		}
	}
	for raw, want := range map[string]string{
		"":       "",
		"   ;  ": "",
		"  CREATE INDEX i ON public.t USING btree (a) ;\n": "CREATE INDEX CONCURRENTLY i ON " +
			"public.t USING btree (a)",
		"create index i on public.t (a)": "create index CONCURRENTLY i on public.t (a)",
	} {
		if got := restoreSQLForDrop(raw); got != want {
			t.Fatalf("restoreSQLForDrop(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRestoreDroppedIndexContract(t *testing.T) {
	contract, ok := ContractForActionType(ActionTypeRestoreDroppedIndex)
	if !ok {
		t.Fatal("restore_dropped_index has no contract")
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("contract invalid: %v", err)
	}
	if contract.BaseRiskTier != "safe" || contract.RollbackClass != "reversible" {
		t.Fatalf("contract = %s/%s, want safe/reversible", contract.BaseRiskTier,
			contract.RollbackClass)
	}
	for _, g := range contract.Guardrails {
		if isApprovalRequiredGuardrail(g) {
			t.Fatalf("restore contract carries an approval guardrail %q", g)
		}
	}
	if got := changeClassForActionType(ActionTypeRestoreDroppedIndex); got !=
		string(policy.ChangeIndex) {
		t.Fatalf("change class = %q, want index", got)
	}
	drop, _ := ContractForActionType("drop_unused_index")
	for _, provider := range drop.ProviderSupport {
		if !containsString(contract.ProviderSupport, provider) {
			t.Fatalf("a drop may run on %s, but its restore may not", provider)
		}
	}
	if dropKindForActionType(ActionTypeRestoreDroppedIndex) != "" {
		t.Fatal("the restore drops nothing")
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestMatchDropRestoreAcceptsEveryDropRule(t *testing.T) {
	for _, c := range dropRuleRollbacks {
		if err := matchDropRestore(c.want, c.rollback, c.drop); err != nil {
			t.Fatalf("%s: %v", c.rule, err)
		}
		// Rows written before executor-01 hold the raw definition.
		if err := matchDropRestore(c.want, restoreSQLForDrop(c.rollback), c.drop); err != nil {
			t.Fatalf("%s (normalized record): %v", c.rule, err)
		}
	}
}

// Invalid input: anything but the recorded definition of the dropped index
// is a mismatch, never authorized.
func TestMatchDropRestoreRefusesEverythingElse(t *testing.T) {
	c := dropRuleRollbacks[0]
	cases := map[string]struct{ restore, rollback, drop string }{
		"other table": {strings.Replace(c.want, "public.orders", "public.payments", 1),
			c.rollback, c.drop},
		"other columns": {strings.Replace(c.want, "(status)", "(status, id)", 1),
			c.rollback, c.drop},
		"raw, not concurrent":  {strings.TrimSuffix(c.rollback, ";"), c.rollback, c.drop},
		"appended statement":   {c.want + "; DROP TABLE public.orders", c.rollback, c.drop},
		"recorded names other": {c.want, c.rollback, `DROP INDEX CONCURRENTLY "public"."x";`},
		"other schema drop": {c.want, c.rollback,
			`DROP INDEX CONCURRENTLY "app"."orders_status_idx";`},
		"forward is no drop": {c.want, c.rollback, "VACUUM public.orders"},
		"empty restore":      {"", c.rollback, c.drop},
		"empty record":       {c.want, "", c.drop},
		"record not create": {"ALTER TABLE public.orders RESET (fillfactor)", "ALTER TABLE " +
			"public.orders RESET (fillfactor)", c.drop},
	}
	for name, in := range cases {
		err := matchDropRestore(in.restore, in.rollback, in.drop)
		if err == nil || !strings.Contains(err.Error(), rollbackSQLMismatch) {
			t.Fatalf("%s: err = %v, want %s", name, err, rollbackSQLMismatch)
		}
	}
}

// restoreGateExecutor is an executor whose real standing gate sees cfg.
func restoreGateExecutor(cfg *config.Config, rampStart time.Time) *Executor {
	e := New(nil, cfg, rampStart, noopExecLog)
	return withTestStandingGate(e)
}

func restoreTrustConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, true
	cfg.Trust.MaintenanceWindow = "always"
	return cfg
}

// Happy path: every drop rule's normalized rollback is executed by the
// real gate at autonomous trust under the restore bypass.
func TestGateDropRestoreExecutesEveryDropRule(t *testing.T) {
	e := restoreGateExecutor(restoreTrustConfig(), time.Now().Add(-90*24*time.Hour))
	for _, c := range dropRuleRollbacks {
		v := e.gateDropRestore(context.Background(), restoreSQLForDrop(c.rollback))
		if !v.Allowed {
			t.Fatalf("%s: restore withheld: %s", c.rule, v.Reason)
		}
	}
}

// State transitions: each trust downgrade still lets the restore run.
func TestGateDropRestoreSurvivesTrustDowngrades(t *testing.T) {
	sql := dropRuleRollbacks[0].want
	cases := map[string]func(*Executor){
		"observation":    func(e *Executor) { _ = e.SetTrustLevel("observation") },
		"approval mode":  func(e *Executor) { e.SetExecutionMode("approval") },
		"tier3_safe off": func(e *Executor) { e.cfg.Trust.Tier3Safe = false },
		"ramp reset":     func(e *Executor) { e.rampStart = time.Now() },
	}
	for name, downgrade := range cases {
		e := restoreGateExecutor(restoreTrustConfig(), time.Now().Add(-90*24*time.Hour))
		downgrade(e)
		if v := e.gateDropRestore(context.Background(), sql); !v.Allowed {
			t.Fatalf("%s: restore withheld: %s", name, v.Reason)
		}
	}
}

// The hard stops bind the restore too; the reason names the stop.
func TestGateDropRestoreHonoursHardStops(t *testing.T) {
	sql := dropRuleRollbacks[0].want
	e := restoreGateExecutor(restoreTrustConfig(), time.Now().Add(-90*24*time.Hour))
	e.SetExecutorEnabled(false)
	v := e.gateDropRestore(context.Background(), sql)
	if v.Allowed || !strings.Contains(v.Reason, string(policy.ReasonExecutorDisabled)) {
		t.Fatalf("disabled executor: verdict = %+v, want withheld naming the stop", v)
	}
	e = restoreGateExecutor(restoreTrustConfig(), time.Now().Add(-90*24*time.Hour))
	e.WithEmergencyStopCheck(func(context.Context) bool { return true })
	v = e.gateDropRestore(context.Background(), sql)
	if v.Allowed || !strings.Contains(v.Reason, string(policy.ReasonEmergencyStop)) {
		t.Fatalf("emergency stop: verdict = %+v, want withheld naming the stop", v)
	}
}

// Nil/empty: no standing gate fails closed.
func TestGateDropRestoreWithoutGateFailsClosed(t *testing.T) {
	e := New(nil, restoreTrustConfig(), time.Now().Add(-90*24*time.Hour), noopExecLog)
	v := e.gateDropRestore(context.Background(), dropRuleRollbacks[0].want)
	if v.Allowed || v.Reason == "" {
		t.Fatalf("verdict = %+v, want withheld with a reason", v)
	}
}

// Invalid input: any other rollback under observation is still observe
// only, and a rollback no contract classifies says so, distinct from a
// policy refusal.
func TestStandingRollbackDeciderReasons(t *testing.T) {
	e := restoreGateExecutor(restoreTrustConfig(), time.Now().Add(-90*24*time.Hour))
	_ = e.SetTrustLevel("observation")
	reloption := analyzer.Finding{ObjectIdentifier: "public.orders",
		RecommendedSQL: "ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)"}
	v := e.standingRollbackDecider(reloption)(context.Background(),
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.2)")
	if v.Allowed || !strings.Contains(v.Reason, string(policy.ReasonObserveOnly)) ||
		!strings.HasPrefix(v.Reason, "standing policy withheld automatic rollback") {
		t.Fatalf("reloption rollback under observation = %+v, want observe_only", v)
	}
	created := analyzer.Finding{ObjectIdentifier: "public.orders",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY orders_b_idx ON public.orders (b)"}
	v = e.standingRollbackDecider(created)(context.Background(),
		"DROP INDEX CONCURRENTLY IF EXISTS public.orders_b_idx")
	if v.Allowed || !strings.Contains(v.Reason, string(policy.ReasonObserveOnly)) {
		t.Fatalf("index revert under observation = %+v, want observe_only", v)
	}
	v = e.standingRollbackDecider(reloption)(context.Background(), "SELECT pg_sleep(1)")
	if v.Allowed || !strings.HasPrefix(v.Reason, rollbackNoContract) {
		t.Fatalf("unclassified rollback = %+v, want %s", v, rollbackNoContract)
	}
	if strings.HasPrefix(v.Reason, "standing policy withheld") {
		t.Fatal("a missing contract reads as a policy refusal")
	}
}

// CHECK-03: every forward contract's rollback classifies to a contract.
func TestForwardRollbacksClassifyToContracts(t *testing.T) {
	if got := unclassifiedRollbacks(forwardRollbackSamples); len(got) != 0 {
		t.Fatalf("unclassified rollbacks: %v", got)
	}
	for _, s := range forwardRollbackSamples {
		if got := ActionTypeForSQL(s.forward); got != s.actionType {
			t.Fatalf("sample forward %q classifies as %q, want %q", s.forward, got,
				s.actionType)
		}
		contract, _ := ContractForActionType(s.actionType)
		if contract.RollbackClass != "reversible" {
			t.Fatalf("sample %s is not a reversible forward contract", s.actionType)
		}
	}
	seen := map[string]bool{}
	for _, s := range forwardRollbackSamples {
		seen[s.actionType] = true
	}
	for _, want := range []string{"drop_unused_index", "create_index_concurrently",
		"set_table_autovacuum", "alter_system_guc", "alter_database_guc",
		"create_statistics", "apply_query_hint"} {
		if !seen[want] {
			t.Fatalf("no rollback sample for reversible contract %s", want)
		}
	}
}

// The self-check finds a rollback that no contract classifies, and logs
// it at ERROR from New; a clean table logs nothing.
func TestRollbackSelfCheckReportsUnclassified(t *testing.T) {
	bad := []rollbackSample{{"drop_unused_index", "DROP INDEX CONCURRENTLY public.x",
		"DROP TABLE public.orders"}, {"create_index_concurrently",
		"CREATE INDEX CONCURRENTLY x ON public.t (a)", "CREATE INDEX x ON public.t (a)"}}
	got := unclassifiedRollbacks(bad)
	if len(got) != 2 || !strings.Contains(got[0], "drop_unused_index") {
		t.Fatalf("unclassified = %v, want both bad samples", got)
	}
	var mu sync.Mutex
	var errors []string
	New(nil, config.DefaultConfig(), time.Time{}, func(c, msg string, _ ...any) {
		mu.Lock()
		defer mu.Unlock()
		if c == "ERROR" {
			errors = append(errors, msg)
		}
	})
	if len(errors) != 0 {
		t.Fatalf("startup self-check logged %v", errors)
	}
}

func TestRestoreQueueIdentity(t *testing.T) {
	key := restoreIdentityKey(42)
	id, ok := restoreQueueActionID(store.QueuedAction{
		ActionType: ActionTypeRestoreDroppedIndex, IdentityKey: key})
	if !ok || id != 42 {
		t.Fatalf("restoreQueueActionID(%q) = %d, %v", key, id, ok)
	}
	for _, item := range []store.QueuedAction{
		{ActionType: "create_index_concurrently", IdentityKey: key},
		{ActionType: ActionTypeRestoreDroppedIndex, IdentityKey: "restore_dropped_index:"},
		{ActionType: ActionTypeRestoreDroppedIndex, IdentityKey: key + "x"},
		{ActionType: ActionTypeRestoreDroppedIndex, IdentityKey: restoreIdentityKey(0)},
		{ActionType: ActionTypeRestoreDroppedIndex,
			IdentityKey: "restore_dropped_index:action:-3"},
		{ActionType: ActionTypeRestoreDroppedIndex},
	} {
		if id, ok := restoreQueueActionID(item); ok {
			t.Fatalf("%+v parsed as restore of action %d", item, id)
		}
	}
}

// The monitor of a drop action uses the restore authorizer and the
// emergency-stop hold; every other action keeps the standing authorizer.
func TestFindingMonitorConfigRoutesDrops(t *testing.T) {
	e := restoreGateExecutor(restoreTrustConfig(), time.Now().Add(-90*24*time.Hour))
	drop := analyzer.Finding{RecommendedSQL: dropRuleRollbacks[0].drop,
		RollbackSQL: dropRuleRollbacks[0].want}
	cfg := e.findingMonitorConfig(drop, 7)
	if cfg.Decide == nil || cfg.HoldForStop == nil {
		t.Fatalf("drop monitor config = %+v, want the restore decider and stop hold", cfg)
	}
	other := analyzer.Finding{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'",
		RollbackSQL: "ALTER SYSTEM RESET work_mem"}
	cfg = e.findingMonitorConfig(other, 8)
	if cfg.Decide == nil || cfg.HoldForStop != nil {
		t.Fatalf("GUC monitor config = %+v, want only the standing decider", cfg)
	}
}

// prepareFindingRollback normalizes a drop's rollback before it is queued
// or run, so the stored rollback and the operator button run the same SQL.
func TestPrepareFindingRollbackNormalizesDrops(t *testing.T) {
	for _, c := range dropRuleRollbacks {
		f := analyzer.Finding{RecommendedSQL: c.drop, RollbackSQL: c.rollback}
		if err := prepareFindingRollback(&f); err != nil {
			t.Fatalf("%s: %v", c.rule, err)
		}
		if f.RollbackSQL != c.want {
			t.Fatalf("%s: rollback = %q, want %q", c.rule, f.RollbackSQL, c.want)
		}
	}
	keep := analyzer.Finding{RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'",
		RollbackSQL: "ALTER SYSTEM RESET work_mem;"}
	if err := prepareFindingRollback(&keep); err != nil ||
		keep.RollbackSQL != "ALTER SYSTEM RESET work_mem;" {
		t.Fatalf("non-drop rollback changed: %q (%v)", keep.RollbackSQL, err)
	}
}
