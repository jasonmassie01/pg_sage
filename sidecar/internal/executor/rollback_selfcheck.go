package executor

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// rollbackSample is a reversible forward action and the rollback its
// producer writes, in the exact form the producer emits it.
type rollbackSample struct {
	actionType, forward, rollback string
}

// forwardRollbackSamples covers every reversible forward contract the
// executor runs on its own: drop rules (pg_get_indexdef + ";"), the
// verified create, the config prior capture (SET to the prior value or
// RESET), pg_sage's statistics and the tuner's hint rows.
var forwardRollbackSamples = []rollbackSample{
	{"drop_unused_index", `DROP INDEX CONCURRENTLY "public"."orders_status_idx";`,
		"CREATE INDEX orders_status_idx ON public.orders USING btree (status);"},
	{"drop_unused_index", `DROP INDEX CONCURRENTLY "public"."users_email_key2";`,
		"CREATE UNIQUE INDEX users_email_key2 ON public.users USING btree (email);"},
	{"create_index_concurrently", "CREATE INDEX CONCURRENTLY orders_b_idx ON public.orders (b)",
		"DROP INDEX CONCURRENTLY IF EXISTS public.orders_b_idx"},
	{"set_table_autovacuum",
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)",
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.2)"},
	{"set_table_autovacuum",
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.02)",
		"ALTER TABLE public.orders RESET (autovacuum_vacuum_scale_factor)"},
	{"alter_system_guc", "ALTER SYSTEM SET work_mem = '64MB'", "ALTER SYSTEM RESET work_mem"},
	{"alter_system_guc", "ALTER SYSTEM SET work_mem = '64MB'",
		"ALTER SYSTEM SET work_mem = '4MB'"},
	{"alter_database_guc", "ALTER DATABASE app SET work_mem = '16MB'",
		"ALTER DATABASE app RESET work_mem"},
	{"create_statistics", "CREATE STATISTICS public.sage_stx_orders_ab " +
		"(ndistinct, dependencies, mcv) ON a, b FROM public.orders",
		"DROP STATISTICS IF EXISTS public.sage_stx_orders_ab"},
	{"apply_query_hint", "INSERT INTO hint_plan.hints (query_id, application_name, hints) " +
		"SELECT 42, '', $h$IndexScan(o)$h$ WHERE NOT EXISTS (SELECT 1 FROM " +
		"hint_plan.hints WHERE query_id = 42 AND application_name = '')",
		"DELETE FROM hint_plan.hints WHERE query_id = 42 AND application_name = ''"},
	{ActionTypeRevokePublicGrant, "REVOKE SELECT, UPDATE ON TABLE public.leads FROM PUBLIC",
		"GRANT SELECT, UPDATE ON TABLE public.leads TO PUBLIC"},
	{ActionTypeEnableRLS, "ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE public.leads DISABLE ROW LEVEL SECURITY"},
}

// rollbackContractFor is the contract the automatic rollback of a forward
// action is authorized under: a drop's restore runs under the restore
// contract when it is a valid CREATE INDEX CONCURRENTLY; any other rollback
// under the contract its SQL classifies to.
func rollbackContractFor(forwardSQL, rollbackSQL string) (ActionContract, bool) {
	if isDropActionSQL(forwardSQL) {
		restore := restoreSQLForDrop(rollbackSQL)
		if ValidateExecutorSQL(restore) != nil ||
			ActionTypeForSQL(restore) != "create_index_concurrently" {
			return ActionContract{}, false
		}
		return restoreDroppedIndexContract(), true
	}
	if exposureActionType(forwardSQL) != "" {
		return exposureRollbackContract(forwardSQL, rollbackSQL)
	}
	return contractForFinding(analyzer.Finding{RecommendedSQL: rollbackSQL})
}

// unclassifiedRollbacks lists the samples whose rollback no contract
// classifies: their automatic rollback would always be withheld.
func unclassifiedRollbacks(samples []rollbackSample) []string {
	var out []string
	for _, s := range samples {
		if _, ok := rollbackContractFor(s.forward, s.rollback); !ok {
			out = append(out, fmt.Sprintf("%s rollback %q", s.actionType, s.rollback))
		}
	}
	return out
}

// checkRollbackContracts is the startup self-check: every forward
// contract's rollback must classify to a contract (logged at ERROR).
func (e *Executor) checkRollbackContracts() {
	for _, missing := range unclassifiedRollbacks(forwardRollbackSamples) {
		e.logFn("ERROR", "executor self-check: the %s classifies to no action contract; "+
			"its automatic rollback would be withheld", missing)
	}
}
