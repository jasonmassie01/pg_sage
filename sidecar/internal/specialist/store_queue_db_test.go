package specialist

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ux-05e: the request audit carries the approval queue id of a cancel
// remediation the gate queued, read from the remediation's proposal
// record, so the dashboard links the request to the approval it waits on.
// No concurrency test: Recent is a read of committed rows.

// seedProposal adds an investigation and its cancel proposal, queued as
// queueID (0 = never queued); it returns the investigation and proposal ids.
func seedProposal(t *testing.T, pool *pgxpool.Pool, queueID int64) (string, string) {
	t.Helper()
	ctx := context.Background()
	var dep, db, inv, pid string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text, gen_random_uuid()::text,
		gen_random_uuid()::text, gen_random_uuid()::text`).Scan(&dep, &db, &inv,
		&pid); err != nil {
		t.Fatal(err)
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sage.sre_database_bindings (deployment_id, database_id, runtime_key,
			identity_strength, cluster_epoch) VALUES ($1, $2, $3, 'configured', 'e1')`,
			[]any{dep, db, "ux05e-" + db}},
		{`INSERT INTO sage.sre_investigations (deployment_id, database_id, id,
			source_case_id, trigger_kind, trigger_fingerprint, state, expires_at)
			VALUES ($1, $2, $3, 'case:ux05e', 'lock_blocking', sha256('ux05e'::bytea),
			'evaluating', clock_timestamp() + interval '1 hour')`, []any{dep, db, inv}},
		{`INSERT INTO sage.sre_action_proposals (id, deployment_id, database_id,
			investigation_id, action_class, state, queue_id, expires_at)
			VALUES ($1, $2, $3, $4, 'cancel_backend', 'requested', NULLIF($5, 0),
			clock_timestamp() + interval '1 hour')`, []any{pid, dep, db, inv, queueID}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed proposal: %v", err)
		}
	}
	return inv, pid
}

func recordRemediation(t *testing.T, st *PGStore, inv, remID, verdict string) string {
	t.Helper()
	tok := uniqueToken("ux05e")
	rec, err := st.Record(context.Background(), Record{Kind: KindRemediation, TokenID: tok,
		IdentityName: "AWS DevOps Agent", Actor: "agent:aws:" + tok, Transport: "http",
		Database: "orders", InvestigationID: inv, RemediationID: remID, Verdict: verdict})
	if err != nil {
		t.Fatalf("record remediation: %v", err)
	}
	return rec.ID
}

func recentByID(t *testing.T, st *PGStore) map[string]Record {
	t.Helper()
	recent, err := st.Recent(context.Background(), 500)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	out := map[string]Record{}
	for _, r := range recent {
		out[r.ID] = r
	}
	return out
}

func TestPGStore_RecentCarriesTheQueuedApproval(t *testing.T) {
	pool := livePool(t)
	st := NewPGStore(pool)
	inv, pid := seedProposal(t, pool, 4242)
	queued := recordRemediation(t, st, inv, cancelPrefix+pid, "queued_for_approval")
	again := recordRemediation(t, st, inv, cancelPrefix+pid, "already_requested")
	blocked := recordRemediation(t, st, inv, cancelPrefix+pid, "blocked")
	got := recentByID(t, st)
	for name, c := range map[string]struct {
		id   string
		want int64
	}{
		"queued":            {queued, 4242},
		"already requested": {again, 4242},
		// The gate refused this request: it queued nothing.
		"blocked": {blocked, 0},
	} {
		rec, ok := got[c.id]
		if !ok {
			t.Fatalf("%s: recent misses record %s", name, c.id)
		}
		if rec.ApprovalQueueID != c.want {
			t.Errorf("%s: queue id %d, want %d", name, rec.ApprovalQueueID, c.want)
		}
		if rec.Kind != KindRemediation || rec.RemediationID != cancelPrefix+pid {
			t.Errorf("%s: record %+v", name, rec)
		}
	}
}

func TestPGStore_RecentQueueIDNeedsAMatchingProposal(t *testing.T) {
	pool := livePool(t)
	st := NewPGStore(pool)
	inv, pid := seedProposal(t, pool, 0)
	otherInv, otherPid := seedProposal(t, pool, 777)
	cases := map[string]string{
		// Nil: the proposal was never queued.
		"never queued": recordRemediation(t, st, inv, cancelPrefix+pid,
			"queued_for_approval"),
		// Hostile or malformed ids never reach the uuid cast.
		"malformed id": recordRemediation(t, st, inv, cancelPrefix+"../../etc",
			"queued_for_approval"),
		"upper-case junk": recordRemediation(t, st, inv, cancelPrefix+"ZZZZZZZZ-ZZZZ-ZZZZ-"+
			"ZZZZ-ZZZZZZZZZZZZ", "queued_for_approval"),
		// A custodian remediation has no cancel proposal record.
		"custodian": recordRemediation(t, st, inv, custodianPrefix+"abc",
			"queued_for_approval"),
		// Another investigation's proposal is not this request's approval.
		"other investigation": recordRemediation(t, st, inv, cancelPrefix+otherPid,
			"queued_for_approval"),
	}
	got := recentByID(t, st)
	for name, id := range cases {
		rec, ok := got[id]
		if !ok {
			t.Fatalf("%s: recent misses record %s", name, id)
		}
		if rec.ApprovalQueueID != 0 {
			t.Errorf("%s: queue id %d, want none", name, rec.ApprovalQueueID)
		}
	}
	// The matching pair still links, so the zeros above are not a dead join.
	match := recordRemediation(t, st, otherInv, cancelPrefix+otherPid, "queued_for_approval")
	if q := recentByID(t, st)[match].ApprovalQueueID; q != 777 {
		t.Fatalf("matching proposal: queue id %d, want 777", q)
	}
}
