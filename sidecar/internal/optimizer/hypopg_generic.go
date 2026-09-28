package optimizer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// preparedWorkloadName names the per-session statement used to plan
// normalized workload queries before PostgreSQL 16.
const preparedWorkloadName = "sage_hypo_workload"

// explainPrepared plans a normalized query ("... = $1") on servers without
// EXPLAIN (GENERIC_PLAN): PREPARE it, then EXPLAIN EXECUTE it with NULLs
// under force_generic_plan, so the plan is generic and the NULLs are never
// folded in as constants. A savepoint keeps the transaction usable on
// failure, and the statement is always deallocated because prepared
// statements outlive rollback on the pooled connection.
func (s *hypopgSession) explainPrepared(ctx context.Context, query string) ([]byte, error) {
	if _, err := s.tx.Exec(ctx, "SAVEPOINT sage_hypo_explain"); err != nil {
		return nil, err
	}
	plan, err := s.explainPreparedSteps(ctx, query)
	if err != nil {
		_, rbErr := s.tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sage_hypo_explain")
		err = errors.Join(err, rbErr)
	}
	deallocErr := s.deallocatePrepared(ctx)
	if err != nil || deallocErr != nil {
		return nil, errors.Join(err, deallocErr)
	}
	return plan, nil
}

func (s *hypopgSession) explainPreparedSteps(ctx context.Context, query string) ([]byte, error) {
	if _, err := s.tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
		return nil, err
	}
	pg := s.tx.Conn().PgConn()
	// Simple protocol: the text is a validated single statement whose $n
	// markers must reach PREPARE unbound.
	if _, err := pg.Exec(ctx, "PREPARE "+preparedWorkloadName+" AS "+query).ReadAll(); err != nil {
		return nil, err
	}
	var params int
	err := s.tx.QueryRow(ctx, `SELECT cardinality(parameter_types)
		FROM pg_catalog.pg_prepared_statements WHERE name = $1`,
		preparedWorkloadName).Scan(&params)
	if err != nil {
		return nil, fmt.Errorf("read prepared parameter count: %w", err)
	}
	sql := "EXPLAIN (FORMAT JSON) EXECUTE " + preparedWorkloadName +
		preparedExplainArgs(params)
	return singleJSONPlan(pg.Exec(ctx, sql).ReadAll())
}

func (s *hypopgSession) deallocatePrepared(ctx context.Context) error {
	var exists bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1
		FROM pg_catalog.pg_prepared_statements WHERE name = $1)`,
		preparedWorkloadName).Scan(&exists)
	if err != nil || !exists {
		return err
	}
	_, err = s.tx.Exec(ctx, "DEALLOCATE "+preparedWorkloadName)
	return err
}

// preparedExplainArgs is the EXECUTE argument list: one NULL per parameter.
func preparedExplainArgs(params int) string {
	if params <= 0 {
		return ""
	}
	return "(" + strings.TrimSuffix(strings.Repeat("NULL, ", params), ", ") + ")"
}
