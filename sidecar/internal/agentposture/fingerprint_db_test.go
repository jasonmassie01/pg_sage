package agentposture

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Temporary tables and views are no posture: creating them in another
// session must not change the fingerprint, or every analyzer cycle of a
// database with temp-table churn re-runs every detector. A permanent
// table still changes it.
func TestFingerprint_IgnoresTempObjectsOfOtherSessions(t *testing.T) {
	pool, ctx := livePool(t)
	other, err := pgx.Connect(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect a second session: %v", err)
	}
	t.Cleanup(func() { _ = other.Close(context.Background()) })
	fp := func() int64 {
		t.Helper()
		var v int64
		readTx(t, ctx, pool, func(tx pgxTx) {
			var err error
			if v, err = Fingerprint(ctx, tx); err != nil {
				t.Fatalf("Fingerprint: %v", err)
			}
		})
		return v
	}
	s := suffix(t)
	n := 0
	stable := false
	for i := 0; i < 4 && !stable; i++ { // cluster-wide roles may change meanwhile
		before := fp()
		for _, stmt := range []string{
			"CREATE TEMP TABLE tmp_" + s + "_" + string(rune('a'+n)) + " (id int)",
			"CREATE TEMP VIEW tmpv_" + s + "_" + string(rune('a'+n)) +
				" AS SELECT 1 AS one",
			"GRANT SELECT ON tmp_" + s + "_" + string(rune('a'+n)) + " TO PUBLIC",
		} {
			if _, err := other.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		n++
		stable = fp() == before
	}
	if !stable {
		t.Fatal("temporary tables and views of another session changed the fingerprint")
	}
	table := "posture_fp_perm_" + s
	before := fp()
	execAll(t, ctx, pool, "CREATE TABLE "+table+" (id int)")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	if fp() == before {
		t.Fatal("a permanent table did not change the fingerprint")
	}
}
