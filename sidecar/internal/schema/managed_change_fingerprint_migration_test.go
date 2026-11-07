package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// clouds-02: managed change fingerprints become v2 (provider, parameter,
// value; no target). The migration collapses duplicate open rows, then
// recomputes every legacy fingerprint, and is a no-op the second time.

func TestManagedChangeFingerprintMigrationIsRegistered(t *testing.T) {
	m := migrationStatements()
	at := slices.Index(m, ddlManagedChangeFingerprintV2)
	if at < 0 {
		t.Fatal("ddlManagedChangeFingerprintV2 is not registered in migrationStatements")
	}
	if at < slices.Index(m, ddlManagedChangeProposals) {
		t.Fatal("the fingerprint migration must run after the table exists")
	}
	for _, want := range []string{"lock_timeout", "NOT LIKE 'v2:%'", "sha256("} {
		if !strings.Contains(ddlManagedChangeFingerprintV2, want) {
			t.Errorf("migration lacks %q", want)
		}
	}
	if strings.Contains(ddlManagedChangeFingerprintV2, "digest(") {
		t.Error("the migration must not need pgcrypto")
	}
}

func v2Fingerprint(provider, parameter, value string) string {
	sum := sha256.Sum256([]byte(provider + "|" + parameter + "|" + value))
	return "v2:" + hex.EncodeToString(sum[:])[:16]
}

type mcRow struct {
	fingerprint, status, inProposal string
	decidedBy                       *int
}

func seedManagedChange(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fp, target,
	value, status, age string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.managed_change_proposals
		(fingerprint, provider, mechanism, target, parameter, value, reboot_required,
		 proposal, status, decided_by, updated_at)
		VALUES ($1, 'rds', 'parameter_group', $2, 'work_mem', $3, false,
		        jsonb_build_object('fingerprint', $1::text, 'target', $2::text), $4,
		        CASE WHEN $4 IN ('approved', 'rejected') THEN 7 END,
		        now() - $5::interval)
		RETURNING id`, fp, target, value, status, age).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", fp, err)
	}
	return id
}

func readManagedChanges(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[int64]mcRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id, fingerprint, status,
		coalesce(proposal->>'fingerprint', ''), decided_by FROM sage.managed_change_proposals`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]mcRow{}
	for rows.Next() {
		var id int64
		var r mcRow
		if err := rows.Scan(&id, &r.fingerprint, &r.status, &r.inProposal,
			&r.decidedBy); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func cleanManagedChanges(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		if _, err := pool.Exec(context.Background(),
			"DELETE FROM sage.managed_change_proposals"); err != nil {
			t.Fatalf("clean managed changes: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
}

// Two open legacy rows for one change under different targets (the
// resolved approval and a placeholder built during an outage) collapse to
// the approved one; closed rows are recomputed too.
func TestManagedChangeFingerprintMigrationCollapsesDuplicates(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanManagedChanges(t, ctx, pool)
	approved := seedManagedChange(t, ctx, pool, "aaaaaaaaaaaaaaaa", "orders-pg16", "65536",
		"approved", "1 hour")
	placeholder := seedManagedChange(t, ctx, pool, "bbbbbbbbbbbbbbbb", "<db-parameter-group>",
		"65536", "pending", "0 seconds")
	rejected := seedManagedChange(t, ctx, pool, "cccccccccccccccc", "orders-pg16", "131072",
		"rejected", "2 days")
	if _, err := pool.Exec(ctx, ddlManagedChangeFingerprintV2); err != nil {
		t.Fatalf("migration: %v", err)
	}
	got := readManagedChanges(t, ctx, pool)
	want := v2Fingerprint("rds", "work_mem", "65536")
	if r := got[approved]; r.status != "approved" || r.fingerprint != want ||
		r.inProposal != want || r.decidedBy == nil || *r.decidedBy != 7 {
		t.Fatalf("approved row = %+v, want approved with %s", r, want)
	}
	if r := got[placeholder]; r.status != "superseded" || r.fingerprint != want {
		t.Fatalf("placeholder duplicate = %+v, want superseded", r)
	}
	if r := got[rejected]; r.status != "rejected" ||
		r.fingerprint != v2Fingerprint("rds", "work_mem", "131072") {
		t.Fatalf("rejected row = %+v, want its v2 fingerprint", r)
	}
	if _, err := pool.Exec(ctx, ddlManagedChangeFingerprintV2); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again := readManagedChanges(t, ctx, pool); !mapsEqual(again, got) {
		t.Fatalf("the second run changed rows:\n%+v\n%+v", got, again)
	}
	var updated int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.managed_change_proposals
		WHERE updated_at > now() - interval '1 second' AND id <> $1`,
		placeholder).Scan(&updated); err != nil || updated != 0 {
		t.Fatalf("rows touched besides the superseded duplicate: %d, %v", updated, err)
	}
}

func mapsEqual(a, b map[int64]mcRow) bool {
	if len(a) != len(b) {
		return false
	}
	for id, r := range a {
		o, ok := b[id]
		if !ok || o.fingerprint != r.fingerprint || o.status != r.status ||
			o.inProposal != r.inProposal {
			return false
		}
	}
	return true
}

// A legacy open row next to an open v2 row of the same change (an old
// binary wrote it after the upgrade) cannot break the unique index: the
// newer pending one is superseded, the approved one kept.
func TestManagedChangeFingerprintMigrationMixedRows(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanManagedChanges(t, ctx, pool)
	want := v2Fingerprint("rds", "work_mem", "65536")
	v2 := seedManagedChange(t, ctx, pool, want, "orders-pg16", "65536", "pending",
		"0 seconds")
	legacy := seedManagedChange(t, ctx, pool, "dddddddddddddddd", "orders-pg16", "65536",
		"approved", "3 hours")
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	got := readManagedChanges(t, ctx, pool)
	if got[legacy].status != "approved" || got[legacy].fingerprint != want ||
		got[v2].status != "superseded" {
		t.Fatalf("rows = %+v, want the legacy approval kept as %s", got, want)
	}
	var legacyLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.managed_change_proposals
		WHERE fingerprint NOT LIKE 'v2:%'`).Scan(&legacyLeft); err != nil || legacyLeft != 0 {
		t.Fatalf("legacy fingerprints left after bootstrap: %d, %v", legacyLeft, err)
	}
}
