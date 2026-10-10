package auditchain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

const genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Every inserted row carries its own link and the hash of its predecessor,
// and an intact chain verifies.
func TestActionLogRowsCarryPredecessorHash(t *testing.T) {
	pool := freshDB(t, "chain_happy")
	ids := []int64{insertAction(t, pool, "CREATE INDEX a"),
		insertAction(t, pool, "CREATE INDEX b"), insertAction(t, pool, "CREATE INDEX c")}
	type chained struct {
		seq        int64
		prev, hash string
	}
	var rows []chained
	for _, id := range ids {
		var c chained
		err := pool.QueryRow(context.Background(), `SELECT chain_seq, chain_prev_hash,
			chain_hash FROM sage.action_log WHERE id = $1`, id).Scan(&c.seq, &c.prev, &c.hash)
		if err != nil {
			t.Fatalf("read chain columns of %d: %v", id, err)
		}
		rows = append(rows, c)
	}
	if rows[0].seq != 1 || rows[0].prev != genesis {
		t.Fatalf("first row = seq %d prev %q, want seq 1 and the genesis hash",
			rows[0].seq, rows[0].prev)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].seq != rows[i-1].seq+1 || rows[i].prev != rows[i-1].hash {
			t.Fatalf("row %d = seq %d prev %q, want seq %d prev %q", i, rows[i].seq,
				rows[i].prev, rows[i-1].seq+1, rows[i-1].hash)
		}
		if len(rows[i].hash) != 64 || rows[i].hash == rows[i-1].hash {
			t.Fatalf("row %d hash %q is not a fresh sha256 hex", i, rows[i].hash)
		}
	}
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || rep.Links != 3 || rep.RowsChecked != 3 || rep.HeadSeq != 3 ||
		rep.HeadHash != rows[2].hash {
		t.Fatalf("intact chain report = %+v", rep)
	}
}

// An outcome change is a link of its own; writing the chain columns into
// the row is not, and neither is an update that changes nothing tracked.
func TestStateChangesAppendLinksOnlyWhenStateChanges(t *testing.T) {
	pool := freshDB(t, "chain_state")
	id := insertAction(t, pool, "CREATE INDEX s")
	if n := linkCount(t, pool, "action_log"); n != 1 {
		t.Fatalf("links after one insert = %d, want 1 (the chain-column write "+
			"must not add a link)", n)
	}
	exec(t, pool, "UPDATE sage.action_log SET outcome = 'success' WHERE id = $1", id)
	exec(t, pool, "UPDATE sage.action_log SET outcome = 'success' WHERE id = $1", id)
	if n := linkCount(t, pool, "action_log"); n != 2 {
		t.Fatalf("links after a change and a no-op update = %d, want 2", n)
	}
	exec(t, pool, `UPDATE sage.action_log SET measured_at = now(),
		after_state = '{"rows": 6}' WHERE id = $1`, id)
	var ops string
	err := pool.QueryRow(context.Background(), `SELECT string_agg(op, '' ORDER BY seq)
		FROM sage.audit_chain_link WHERE chain = 'action_log' AND row_id = $1`, id).Scan(&ops)
	if err != nil {
		t.Fatalf("read ops: %v", err)
	}
	if ops != "IUU" {
		t.Fatalf("ops of the row = %q, want IUU", ops)
	}
	if rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{}); !rep.OK() {
		t.Fatalf("legitimate updates broke the chain: %+v", rep.Problems)
	}
}

// Insert and update of the same row in one transaction: both links, in
// order, and the chain verifies.
func TestInsertAndUpdateInOneTransaction(t *testing.T) {
	pool := freshDB(t, "chain_onetx")
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome) VALUES ('vacuum', 'VACUUM t', 'pending') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE sage.action_log SET outcome = 'failed', "+
		"rollback_reason = 'lock timeout' WHERE id = $1", id); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	links, err := auditchain.LinksForRow(ctx, pool, "action_log", id)
	if err != nil {
		t.Fatalf("links for row: %v", err)
	}
	if len(links) != 2 || links[0].Op != "I" || links[1].Op != "U" ||
		links[1].PrevHash != links[0].Hash {
		t.Fatalf("links = %+v, want I then U chained", links)
	}
	if !strings.Contains(links[1].State, "lock timeout") {
		t.Fatalf("U link state %q does not record the new rollback reason", links[1].State)
	}
	if rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{}); !rep.OK() {
		t.Fatalf("chain problems: %+v", rep.Problems)
	}
}

// A rolled-back write leaves no link and no gap.
func TestRollbackLeavesNoGap(t *testing.T) {
	pool := freshDB(t, "chain_rollback")
	insertAction(t, pool, "CREATE INDEX r1")
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM r')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	insertAction(t, pool, "CREATE INDEX r2")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || rep.Links != 2 || rep.HeadSeq != 2 {
		t.Fatalf("after a rollback: %+v", rep)
	}
}

// Editing a sealed column (what was executed) is detected on that row.
func TestEditOfSealedColumnIsDetected(t *testing.T) {
	pool := freshDB(t, "chain_edit")
	insertAction(t, pool, "CREATE INDEX e1")
	id := insertAction(t, pool, "CREATE INDEX e2")
	exec(t, pool, "UPDATE sage.action_log SET sql_executed = 'SELECT 1' WHERE id = $1", id)
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if rep.OK() || !containsKind(problemKinds(rep)[id], "row_edited") {
		t.Fatalf("sealed edit not reported on row %d: %+v", id, rep.Problems)
	}
}

// Editing the outcome behind the triggers is detected: the live state no
// longer matches the row's latest link.
func TestStateEditBehindTriggersIsDetected(t *testing.T) {
	pool := freshDB(t, "chain_stateedit")
	id := insertAction(t, pool, "CREATE INDEX st")
	exec(t, pool, "UPDATE sage.action_log SET outcome = 'rolled_back' WHERE id = $1", id)
	bypass(t, pool, "UPDATE sage.action_log SET outcome = 'success' WHERE id = $1", id)
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !containsKind(problemKinds(rep)[id], "state_edited") {
		t.Fatalf("state edit not reported: %+v", rep.Problems)
	}
}

// A row deleted behind the triggers is missing; one deleted normally is
// recorded by a delete link and the chain still verifies.
func TestDeletionDetection(t *testing.T) {
	pool := freshDB(t, "chain_delete")
	kept := insertAction(t, pool, "CREATE INDEX d1")
	pruned := insertAction(t, pool, "CREATE INDEX d2")
	exec(t, pool, "DELETE FROM sage.action_log WHERE id = $1", pruned)
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || rep.Deleted != 1 || rep.Links != 3 {
		t.Fatalf("recorded delete: %+v", rep)
	}
	bypass(t, pool, "DELETE FROM sage.action_log WHERE id = $1", kept)
	rep = verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !containsKind(problemKinds(rep)[kept], "row_missing") {
		t.Fatalf("silent delete not reported: %+v", rep.Problems)
	}
}

// Removing a link leaves a gap; editing one breaks its hash; swapping two
// breaks the order.
func TestLinkTamperingIsDetected(t *testing.T) {
	cases := []struct {
		name, sql, kind string
	}{
		{"deleted link", "DELETE FROM sage.audit_chain_link WHERE chain = 'action_log' " +
			"AND seq = 2", "gap"},
		{"edited link", "UPDATE sage.audit_chain_link SET state = replace(state, " +
			"'pending', 'success') WHERE chain = 'action_log' AND seq = 2", "hash_mismatch"},
		{"reordered links", "UPDATE sage.audit_chain_link SET seq = -seq " +
			"WHERE chain = 'action_log' AND seq IN (2, 3); " +
			"UPDATE sage.audit_chain_link SET seq = CASE seq WHEN -2 THEN 3 ELSE 2 END " +
			"WHERE chain = 'action_log' AND seq IN (-2, -3)", "prev_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := freshDB(t, "chain_tamper_"+strings.ReplaceAll(tc.name, " ", "_"))
			for _, s := range []string{"CREATE INDEX t1", "CREATE INDEX t2",
				"CREATE INDEX t3", "CREATE INDEX t4"} {
				insertAction(t, pool, s)
			}
			exec(t, pool, tc.sql)
			rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
			if rep.OK() || !hasKind(rep, tc.kind) {
				t.Fatalf("%s: want a %s problem, got %+v", tc.name, tc.kind, rep.Problems)
			}
		})
	}
}

// A row written with the triggers bypassed carries no link.
func TestUnchainedRowIsDetected(t *testing.T) {
	pool := freshDB(t, "chain_unchained")
	insertAction(t, pool, "CREATE INDEX u1")
	bypass(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM hidden')`)
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !hasKind(rep, "unchained_row") {
		t.Fatalf("unchained row not reported: %+v", rep.Problems)
	}
}

// Rows that existed before the chain was installed are legacy, not
// problems, and installing twice changes nothing.
func TestLegacyRowsAndIdempotentInstall(t *testing.T) {
	pool := freshDB(t, "chain_legacy")
	exec(t, pool, "DROP TRIGGER audit_chain_v1_id ON sage.action_log")
	exec(t, pool, "DROP TRIGGER audit_chain_v1_u ON sage.action_log")
	exec(t, pool, "DROP TRIGGER audit_chain_v1_t ON sage.action_log")
	exec(t, pool, "DELETE FROM sage.audit_chain_meta WHERE chain = 'action_log'")
	exec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM old1'), ('vacuum', 'VACUUM old2')`)
	exec(t, pool, auditchain.MigrationSQL())
	exec(t, pool, auditchain.MigrationSQL())
	insertAction(t, pool, "CREATE INDEX new1")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || rep.LegacyRows != 2 || rep.Links != 1 {
		t.Fatalf("legacy install: %+v", rep)
	}
	var triggers int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_trigger
		WHERE tgrelid = 'sage.action_log'::regclass AND tgname LIKE 'audit_chain_%'`).
		Scan(&triggers)
	if err != nil || triggers != 3 {
		t.Fatalf("chain triggers after two installs = %d (%v), want 3", triggers, err)
	}
}

// TRUNCATE is recorded, and the rows it removed are explained by it.
func TestTruncateIsRecorded(t *testing.T) {
	pool := freshDB(t, "chain_truncate")
	insertAction(t, pool, "CREATE INDEX tr1")
	insertAction(t, pool, "CREATE INDEX tr2")
	exec(t, pool, "TRUNCATE sage.action_log CASCADE")
	insertAction(t, pool, "CREATE INDEX tr3")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || rep.Truncations != 1 || rep.Links != 4 {
		t.Fatalf("truncate: %+v", rep)
	}
}

// A window verifies its own links and rows and ignores tampering outside.
func TestWindowVerification(t *testing.T) {
	pool := freshDB(t, "chain_window")
	var ids []int64
	for _, s := range []string{"w1", "w2", "w3", "w4", "w5"} {
		ids = append(ids, insertAction(t, pool, "CREATE INDEX "+s))
	}
	exec(t, pool, "UPDATE sage.action_log SET sql_executed = 'x' WHERE id = $1", ids[0])
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{FromSeq: 3, ToSeq: 5})
	if !rep.OK() || rep.Links != 3 || rep.FromSeq != 3 || rep.ToSeq != 5 {
		t.Fatalf("window after an edit outside it: %+v", rep)
	}
	exec(t, pool, "UPDATE sage.action_log SET sql_executed = 'y' WHERE id = $1", ids[3])
	rep = verify(t, pool, auditchain.ActionLog, auditchain.Window{FromSeq: 3, ToSeq: 5})
	if !containsKind(problemKinds(rep)[ids[3]], "row_edited") {
		t.Fatalf("edit inside the window not reported: %+v", rep.Problems)
	}
}

// Invalid windows and specs are refused with distinguishable errors.
func TestVerifyRejectsInvalidInput(t *testing.T) {
	pool := freshDB(t, "chain_invalid")
	ctx := context.Background()
	if _, err := auditchain.Verify(ctx, pool, auditchain.ActionLog,
		auditchain.Window{FromSeq: 5, ToSeq: 2}); err == nil ||
		!strings.Contains(err.Error(), "window") {
		t.Fatalf("inverted window: err = %v", err)
	}
	if _, err := auditchain.Verify(ctx, pool, auditchain.ActionLog,
		auditchain.Window{FromSeq: -1}); err == nil ||
		!strings.Contains(err.Error(), "window") {
		t.Fatalf("negative window: err = %v", err)
	}
	if _, err := auditchain.Verify(ctx, pool, auditchain.Spec{},
		auditchain.Window{}); err == nil || !strings.Contains(err.Error(), "spec") {
		t.Fatalf("empty spec: err = %v", err)
	}
	if _, err := auditchain.Verify(ctx, nil, auditchain.ActionLog,
		auditchain.Window{}); err == nil {
		t.Fatalf("nil querier accepted")
	}
}

// An empty chain verifies with nothing checked.
func TestEmptyChainVerifies(t *testing.T) {
	pool := freshDB(t, "chain_empty")
	rep := verify(t, pool, auditchain.ConfigAudit, auditchain.Window{})
	if !rep.OK() || rep.Links != 0 || rep.HeadSeq != 0 || rep.HeadHash != "" {
		t.Fatalf("empty chain: %+v", rep)
	}
}

// The append-only audit tables are chained too: an edited auth event and
// an edited config change are detected.
func TestAppendOnlyAuditTablesAreChained(t *testing.T) {
	pool := freshDB(t, "chain_auth")
	exec(t, pool, `INSERT INTO sage.auth_audit (event, actor_user_id, target_user_id,
		detail, source_ip) VALUES ('login_failed', 0, 7, '{"reason":"bad"}', '10.0.0.1')`)
	exec(t, pool, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('login_succeeded', '{}')`)
	exec(t, pool, `INSERT INTO sage.config_audit (key, old_value, new_value)
		VALUES ('trust.level', 'observation', 'advisory')`)
	for _, spec := range []auditchain.Spec{auditchain.AuthAudit, auditchain.ConfigAudit} {
		if rep := verify(t, pool, spec, auditchain.Window{}); !rep.OK() || rep.Links == 0 {
			t.Fatalf("%s intact: %+v", spec.Chain, rep)
		}
	}
	exec(t, pool, `UPDATE sage.auth_audit SET event = 'login_succeeded'
		WHERE event = 'login_failed'`)
	exec(t, pool, `UPDATE sage.config_audit SET new_value = 'autonomous'`)
	for _, spec := range []auditchain.Spec{auditchain.AuthAudit, auditchain.ConfigAudit} {
		if rep := verify(t, pool, spec, auditchain.Window{}); !hasKind(rep, "row_edited") {
			t.Fatalf("%s edit not detected: %+v", spec.Chain, rep.Problems)
		}
	}
	if n := linkCount(t, pool, "auth_audit"); n != 2 {
		t.Fatalf("auth_audit links = %d, want 2 (append-only tables chain inserts only)", n)
	}
}

// Many tampered rows are reported up to the cap, and the report says so.
func TestProblemListIsCapped(t *testing.T) {
	pool := freshDB(t, "chain_cap")
	exec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		SELECT 'vacuum', 'VACUUM t' || g FROM generate_series(1, $1) g`,
		auditchain.MaxProblems+20)
	exec(t, pool, "UPDATE sage.action_log SET sql_executed = sql_executed || ' x'")
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if len(rep.Problems) != auditchain.MaxProblems || !rep.ProblemsTruncated ||
		rep.Links != auditchain.MaxProblems+20 {
		t.Fatalf("problems = %d truncated=%v links=%d", len(rep.Problems),
			rep.ProblemsTruncated, rep.Links)
	}
}

// A dropped connection surfaces as an error naming the chain, never as a
// clean report.
func TestVerifyPropagatesQueryErrors(t *testing.T) {
	pool := freshDB(t, "chain_err")
	insertAction(t, pool, "CREATE INDEX err1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := auditchain.Verify(ctx, pool, auditchain.ActionLog, auditchain.Window{})
	if err == nil || !strings.Contains(err.Error(), "action_log") {
		t.Fatalf("cancelled verify: err = %v", err)
	}
}

func containsKind(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
