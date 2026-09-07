package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/verify"
)

func TestRetainedCleanupResolvesTerminatedAndQuotedIndexNames(t *testing.T) {
	cases := []struct{ name, target, rename, suffix string }{
		{"semicolon", "public.cleanup_old", "", ";"},
		{"quoted space", `public."cleanup old"`, `"cleanup old"`, ";"},
		{"quoted quote", `public."cleanup""old"`, `"cleanup""old"`, " RESTRICT;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drop := "DROP INDEX CONCURRENTLY IF EXISTS " + tc.target + tc.suffix
			e, id, gate := retainedFixture(t, true, drop)
			if tc.rename != "" {
				rename := "ALTER INDEX cleanup_old RENAME TO " + tc.rename
				if _, err := e.pool.Exec(t.Context(), rename); err != nil {
					t.Fatal(err)
				}
			}
			state := map[string]any{}
			err := e.snapshotSupersededIndex(t.Context(), analyzer.Finding{
				RecommendedSQL: "CREATE INDEX cleanup_new ON public.cleanup_orders(id)",
				Detail:         map[string]any{"drop_ddl": drop}}, state)
			if err != nil || state["superseded_index_drop_sql"] != drop {
				t.Fatalf("valid cleanup intent was not preserved: state=%v error=%v", state, err)
			}
			if err := (&executorIndexActions{exec: e}).Retain(t.Context(), id,
				verify.Verdict{Retain: true}); !errors.Is(err, ErrReviewedCleanupRequired) {
				t.Fatalf("valid quoted cleanup did not require review: %v", err)
			}
			var exists bool
			if err := e.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", tc.target).
				Scan(&exists); err != nil || !exists || gate.calls != 1 {
				t.Fatalf("cleanup exists=%v policy calls=%d error=%v", exists, gate.calls, err)
			}
		})
	}
}

// This syntax boundary is stateless; concurrent cleanup uses real PostgreSQL in existing tests.
func TestRetainedCleanupRejectsExtraTargetsAndClauses(t *testing.T) {
	for _, sql := range []string{
		"", "DROP INDEX public.cleanup_old",
		"DROP INDEX CONCURRENTLY public.cleanup_old, public.cleanup_new",
		"DROP INDEX CONCURRENTLY public.cleanup_old CASCADE",
		"DROP INDEX CONCURRENTLY public.cleanup_old extra",
		"DROP INDEX CONCURRENTLY public.cleanup_old; SELECT 1",
	} {
		if err := validateSupersededDrop(sql); err == nil {
			t.Errorf("unsafe or malformed cleanup accepted: %q", sql)
		}
	}
}

func TestRetainedCleanupSnapshotPropagatesCancellation(t *testing.T) {
	e, _, _ := retainedFixture(t, true, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	state := map[string]any{}
	err := e.snapshotSupersededIndex(ctx, analyzer.Finding{
		RecommendedSQL: "CREATE INDEX cleanup_new ON public.cleanup_orders(id)",
		Detail:         map[string]any{"drop_ddl": "DROP INDEX CONCURRENTLY public.cleanup_old"}}, state)
	if !errors.Is(err, context.Canceled) || len(state) != 0 {
		t.Fatalf("cleanup target lost cancellation or wrote state: %v %v", state, err)
	}
}
