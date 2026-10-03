package executor

import (
	"errors"
	"testing"
)

// The finding's rollback only identifies the created index; the executed
// rollback is re-derived schema-qualified (prepareVerifiedIndex). An
// unqualified identifying DROP must therefore be accepted when it names the
// created index, while the executed-SQL rule (schema-qualified destructive
// DDL) still applies to what actually runs.
func TestRollbackTargetsCreatedIndex_UnqualifiedIdentityAccepted(t *testing.T) {
	create := "CREATE INDEX CONCURRENTLY shp_b01_id_idx ON public.shp_b01 (id)"
	for _, rollback := range []string{
		"DROP INDEX CONCURRENTLY shp_b01_id_idx",
		"DROP INDEX CONCURRENTLY IF EXISTS shp_b01_id_idx;",
		"DROP INDEX CONCURRENTLY public.shp_b01_id_idx",
	} {
		if err := rollbackTargetsCreatedIndex(create, rollback); err != nil {
			t.Errorf("rollbackTargetsCreatedIndex(%q) = %v, want nil", rollback, err)
		}
	}
}

func TestRollbackTargetsCreatedIndex_RejectsOtherTargets(t *testing.T) {
	create := "CREATE INDEX CONCURRENTLY shp_b01_id_idx ON public.shp_b01 (id)"
	cases := map[string]string{
		"other index":     "DROP INDEX CONCURRENTLY other_idx",
		"other schema":    "DROP INDEX CONCURRENTLY sage.shp_b01_id_idx",
		"two statements":  "DROP INDEX CONCURRENTLY shp_b01_id_idx; DROP TABLE public.t",
		"not concurrent":  "DROP INDEX shp_b01_id_idx",
		"not a drop":      "SELECT 1",
	}
	for name, rollback := range cases {
		err := rollbackTargetsCreatedIndex(create, rollback)
		if err == nil || !errors.Is(err, ErrVerificationUnavailable) {
			t.Errorf("%s: rollbackTargetsCreatedIndex(%q) = %v, want ErrVerificationUnavailable",
				name, rollback, err)
		}
	}
}
