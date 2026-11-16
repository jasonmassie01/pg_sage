package changeplan

import (
	"strings"
	"testing"
)

// plan-02b: steps that are never automatic at any trust level. Approve-all
// never covers them; the engine stops before each with awaiting_approval.
// No concurrent access tests: NeedsOwnApproval is a pure function.

// actionStep is an action step running sql under actionType.
func actionStep(actionType, sql, rollback string) Step {
	return Step{Ordinal: 1, Kind: KindAction, ActionType: actionType, ProposedSQL: sql,
		RollbackSQL: rollback}
}

var testSettings = SettingContexts{"random_page_cost": "user",
	"shared_buffers": "postmaster", "work_mem": "user", "max_connections": "postmaster"}

func settingAction(name, sql string) Step {
	s := actionStep("alter_system_guc", sql, "ALTER SYSTEM RESET "+name)
	s.Setting = name
	return s
}

// neverAutomatic is one case per never-automatic class.
var neverAutomatic = []struct {
	name   string
	step   Step
	reason string
}{
	{"not reversible", actionStep("terminate_backend",
		"SELECT pg_terminate_backend(4242)", ""), ReasonRollbackClass},
	{"mitigation only", actionStep("cancel_backend",
		"SELECT pg_cancel_backend(4242)", ""), ReasonRollbackClass},
	{"forward fix only", actionStep("alter_table",
		"ALTER TABLE public.orders ADD CONSTRAINT c CHECK (id > 0) NOT VALID", ""),
		ReasonRollbackClass},
	{"table rewrite", actionStep("vacuum_table", "VACUUM FULL public.orders", ""),
		ReasonTableRewrite},
	{"cluster rewrite", actionStep("vacuum_table",
		"CLUSTER public.orders USING orders_pkey", ""), ReasonTableRewrite},
	{"partition conversion", actionStep("alter_table",
		"ALTER TABLE public.orders ATTACH PARTITION public.orders_2026 FOR VALUES "+
			"FROM ('2026-01-01') TO ('2027-01-01')", ""), ReasonPartition},
	{"type change", actionStep("alter_table",
		"ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric(12,2)", ""),
		ReasonTypeChange},
	{"dropped column", actionStep("alter_table",
		"ALTER TABLE public.orders DROP COLUMN note", ""), ReasonDroppedData},
	{"dropped table", actionStep("alter_table", "DROP TABLE public.old_orders", ""),
		ReasonDroppedData},
	{"deleted rows", actionStep("retention_delete",
		"DELETE FROM public.events WHERE created_at < now() - interval '90 days'", ""),
		ReasonDroppedData},
	{"truncate", actionStep("retention_delete", "TRUNCATE public.events", ""),
		ReasonDroppedData},
	{"restart-bound setting", settingAction("shared_buffers",
		"ALTER SYSTEM SET shared_buffers = '256MB'"), ReasonRestartBound},
	{"restart-bound setting named only in the SQL", actionStep("alter_system_guc",
		"ALTER SYSTEM SET max_connections = 200", "ALTER SYSTEM RESET max_connections"),
		ReasonRestartBound},
	{"setting of unknown context", settingAction("pg_sage_custom.knob",
		"ALTER SYSTEM SET pg_sage_custom.knob = 1"), ReasonSettingUnknown},
	{"reset all", actionStep("alter_system_guc", "ALTER SYSTEM RESET ALL", ""),
		ReasonSettingUnknown},
	{"promote", actionStep("failover_instance", "SELECT pg_promote()", ""),
		ReasonTopology},
	{"subscription", actionStep("alter_table",
		"ALTER SUBSCRIPTION orders_sub DISABLE", ""), ReasonTopology},
	{"instance action", actionStep("resize_instance", "", ""), ReasonTopology},
	{"privilege revoke", actionStep("guard_revoke",
		"REVOKE SELECT ON public.orders FROM reporting", ""), ReasonPrivilegeRevoke},
	{"pooler mode", actionStep("set_pooler_mode", "SET pool_mode = 'transaction'", ""),
		ReasonPoolerMode},
	{"no contract", actionStep("made_up_action", "COMMENT ON TABLE public.orders IS 'x'",
		""), ReasonNoContract},
	{"reversible without its rollback", actionStep("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY idx_a ON public.orders (a)", ""), ReasonRollbackMissing},
	{"hidden statement", actionStep("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY idx_a ON public.orders (a); DROP TABLE public.orders",
		"DROP INDEX CONCURRENTLY public.idx_a"), ReasonDroppedData},
}

func TestNeedsOwnApproval_EveryNeverAutomaticClass(t *testing.T) {
	for _, c := range neverAutomatic {
		got, why := NeedsOwnApproval(c.step, testSettings)
		if !got || !strings.HasPrefix(why, c.reason+":") {
			t.Errorf("%s: (%v, %q), want true with reason %s", c.name, got, why, c.reason)
		}
	}
}

func TestNeedsOwnApproval_AutomaticSteps(t *testing.T) {
	reloadable := actionStep("alter_system_guc", "ALTER SYSTEM SET random_page_cost = 1.1",
		"ALTER SYSTEM RESET random_page_cost")
	reloadable.Setting = "random_page_cost"
	cases := map[string]Step{
		"create index": actionStep("create_index_concurrently",
			"CREATE INDEX CONCURRENTLY idx_a ON public.orders (customer_id)",
			"DROP INDEX CONCURRENTLY public.idx_a"),
		"drop index with its saved definition": actionStep("drop_unused_index",
			"DROP INDEX CONCURRENTLY public.idx_old",
			"CREATE INDEX CONCURRENTLY idx_old ON public.orders (created_at)"),
		"reloadable setting": reloadable,
		"database setting": actionStep("alter_database_guc",
			"ALTER DATABASE app SET work_mem = '64MB'", "ALTER DATABASE app RESET work_mem"),
		"analyze":    actionStep("analyze_table", "ANALYZE public.orders", ""),
		"check step": {Ordinal: 1, Kind: KindCheck},
	}
	for name, s := range cases {
		if got, why := NeedsOwnApproval(s, testSettings); got || why != "" {
			t.Errorf("%s: (%v, %q), want automatic", name, got, why)
		}
	}
}

// Nil/empty: no setting contexts make every setting step need its own
// approval (fail closed); an empty action step is unclassifiable.
func TestNeedsOwnApproval_NoContextsFailClosed(t *testing.T) {
	s := actionStep("alter_system_guc", "ALTER SYSTEM SET random_page_cost = 1.1",
		"ALTER SYSTEM RESET random_page_cost")
	if got, why := NeedsOwnApproval(s, nil); !got ||
		!strings.HasPrefix(why, ReasonSettingUnknown+":") {
		t.Fatalf("no contexts: (%v, %q), want %s", got, why, ReasonSettingUnknown)
	}
	if got, why := NeedsOwnApproval(Step{Kind: KindAction}, nil); !got ||
		!strings.HasPrefix(why, ReasonNoContract+":") {
		t.Fatalf("empty action step: (%v, %q), want %s", got, why, ReasonNoContract)
	}
}

// An operator step is never run by pg_sage (awaiting_operator), so the
// classification is not its gate.
func TestNeedsOwnApproval_OperatorStepIsNotClassified(t *testing.T) {
	s := Step{Ordinal: 1, Kind: KindOperator, ProposedSQL: "VACUUM FULL public.orders"}
	if got, why := NeedsOwnApproval(s, nil); got || why != "" {
		t.Fatalf("operator step: (%v, %q), want false", got, why)
	}
}

func TestSettingNames_ComeFromTheStepOrItsSQL(t *testing.T) {
	cases := map[string]Step{
		"shared_buffers": {Kind: KindAction, Setting: "Shared_Buffers",
			ProposedSQL: "ALTER SYSTEM SET shared_buffers = '1GB'"},
		"max_connections": {Kind: KindAction,
			ProposedSQL: "alter system set MAX_CONNECTIONS to 300"},
		"work_mem": {Kind: KindAction, ProposedSQL: `ALTER ROLE app SET "work_mem" = '4MB'`},
		"":         {Kind: KindAction, ProposedSQL: "CREATE INDEX CONCURRENTLY i ON t (a)"},
	}
	for want, s := range cases {
		if got := stepSettingName(s); got != want {
			t.Errorf("setting of %q = %q, want %q", s.ProposedSQL, got, want)
		}
	}
}
