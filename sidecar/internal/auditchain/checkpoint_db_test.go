package auditchain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// checkpointAfterFull verifies spec in full, requires it clean and stores
// its checkpoint, returning it as LatestCheckpoint reads it back.
func checkpointAfterFull(t *testing.T, pool *pgxpool.Pool,
	spec auditchain.Spec) auditchain.Checkpoint {
	t.Helper()
	ctx := context.Background()
	rep, err := auditchain.VerifyFromCheckpoint(ctx, pool, spec, true)
	if err != nil || !rep.OK() || rep.Mode != auditchain.ModeFull {
		t.Fatalf("full verify of %s: mode %q, problems %+v, err %v", spec.Chain, rep.Mode,
			rep.Problems, err)
	}
	if err := auditchain.StoreCheckpoint(ctx, pool, rep); err != nil {
		t.Fatalf("store checkpoint: %v", err)
	}
	cp, found, err := auditchain.LatestCheckpoint(ctx, pool, spec.Chain)
	if err != nil || !found || cp.Seq != rep.HeadSeq || cp.Mode != auditchain.ModeFull {
		t.Fatalf("checkpoint after full = %+v found=%v err=%v, want seq %d full", cp,
			found, err, rep.HeadSeq)
	}
	return cp
}

func fromCheckpoint(t *testing.T, pool *pgxpool.Pool, spec auditchain.Spec,
	full bool) auditchain.Report {
	t.Helper()
	rep, err := auditchain.VerifyFromCheckpoint(context.Background(), pool, spec, full)
	if err != nil {
		t.Fatalf("verify %s from checkpoint (full=%v): %v", spec.Chain, full, err)
	}
	return rep
}

// Boundary: from a checkpoint, an edit of a row before it is not seen by
// the incremental pass (it reads only later links) but is by a full one;
// an edit of a row after it is found by both.
func TestIncrementalFromCheckpointBoundary(t *testing.T) {
	pool := freshDB(t, "chain_cp_boundary")
	early := insertAction(t, pool, "CREATE INDEX cp1")
	insertAction(t, pool, "CREATE INDEX cp2")
	cp := checkpointAfterFull(t, pool, auditchain.ActionLog)
	late := insertAction(t, pool, "CREATE INDEX cp3")
	insertAction(t, pool, "CREATE INDEX cp4")
	rep := fromCheckpoint(t, pool, auditchain.ActionLog, false)
	if !rep.OK() || rep.Mode != auditchain.ModeIncremental || rep.FromSeq != cp.Seq+1 ||
		rep.Links != 2 || rep.CheckpointSeq != cp.Seq || rep.RowsChecked != 2 {
		t.Fatalf("clean incremental from %d: %+v", cp.Seq, rep)
	}
	exec(t, pool, "UPDATE sage.action_log SET sql_executed = 'SELECT 1' WHERE id = $1",
		early)
	if rep := fromCheckpoint(t, pool, auditchain.ActionLog, false); !rep.OK() {
		t.Fatalf("incremental reported an edit before its checkpoint: %+v", rep.Problems)
	}
	rep = fromCheckpoint(t, pool, auditchain.ActionLog, true)
	if rep.Mode != auditchain.ModeFull || !containsKind(problemKinds(rep)[early],
		auditchain.KindRowEdited) {
		t.Fatalf("full pass missed the edit before the checkpoint: %+v", rep)
	}
	exec(t, pool, "UPDATE sage.action_log SET sql_executed = 'SELECT 2' WHERE id = $1",
		late)
	rep = fromCheckpoint(t, pool, auditchain.ActionLog, false)
	if !containsKind(problemKinds(rep)[late], auditchain.KindRowEdited) ||
		containsKind(problemKinds(rep)[early], auditchain.KindRowEdited) {
		t.Fatalf("incremental after an edit past the checkpoint: %+v", rep.Problems)
	}
}

// A state change behind the triggers of a row inserted before the
// checkpoint but updated after it is found incrementally: its last link
// lies in the window, so no whole-chain history is needed.
func TestIncrementalChecksStateOfOlderRows(t *testing.T) {
	pool := freshDB(t, "chain_cp_state")
	id := insertAction(t, pool, "CREATE INDEX cps")
	checkpointAfterFull(t, pool, auditchain.ActionLog)
	exec(t, pool, "UPDATE sage.action_log SET outcome = 'success' WHERE id = $1", id)
	if rep := fromCheckpoint(t, pool, auditchain.ActionLog, false); !rep.OK() ||
		rep.Links != 1 || rep.RowsChecked != 1 {
		t.Fatalf("clean state update after the checkpoint: %+v", rep)
	}
	bypass(t, pool, "UPDATE sage.action_log SET outcome = 'rolled_back' WHERE id = $1", id)
	rep := fromCheckpoint(t, pool, auditchain.ActionLog, false)
	if !containsKind(problemKinds(rep)[id], auditchain.KindStateEdited) {
		t.Fatalf("state edit behind the chain not found incrementally: %+v", rep.Problems)
	}
}

// The checkpointed link must still carry its hash: a rewritten, deleted
// or cut-off checkpoint link is a checkpoint_mismatch, in incremental and
// in full mode alike.
func TestCheckpointMismatchIsReported(t *testing.T) {
	cases := []struct {
		name, sql, detail string
	}{
		{"rewritten link", `UPDATE sage.audit_chain_link SET hash = repeat('a', 64)
			WHERE chain = 'action_log' AND seq = $1`, "hash"},
		{"deleted link", `DELETE FROM sage.audit_chain_link
			WHERE chain = 'action_log' AND seq = $1`, "missing"},
		{"head below the checkpoint", `DELETE FROM sage.audit_chain_link
			WHERE chain = 'action_log' AND seq >= $1`, "below"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := freshDB(t, "chain_cp_mismatch")
			insertAction(t, pool, "CREATE INDEX m1")
			insertAction(t, pool, "CREATE INDEX m2")
			cp := checkpointAfterFull(t, pool, auditchain.ActionLog)
			insertAction(t, pool, "CREATE INDEX m3")
			exec(t, pool, tc.sql, cp.Seq)
			for _, full := range []bool{false, true} {
				rep := fromCheckpoint(t, pool, auditchain.ActionLog, full)
				p, ok := problemOf(rep, auditchain.KindCheckpointMismatch)
				if !ok || p.Seq != cp.Seq || !strings.Contains(p.Detail, tc.detail) {
					t.Fatalf("full=%v: problems %+v, want checkpoint_mismatch at %d "+
						"naming %q", full, rep.Problems, cp.Seq, tc.detail)
				}
			}
		})
	}
}

// With no link written since the checkpoint, the head itself is the
// checkpointed link and is compared directly.
func TestCheckpointAtHeadIsCompared(t *testing.T) {
	pool := freshDB(t, "chain_cp_head")
	insertAction(t, pool, "CREATE INDEX h1")
	cp := checkpointAfterFull(t, pool, auditchain.ActionLog)
	rep := fromCheckpoint(t, pool, auditchain.ActionLog, false)
	if !rep.OK() || rep.Links != 0 || rep.HeadSeq != cp.Seq {
		t.Fatalf("incremental with nothing new: %+v", rep)
	}
	exec(t, pool, `UPDATE sage.audit_chain_link SET hash = repeat('b', 64)
		WHERE chain = 'action_log' AND seq = $1`, cp.Seq)
	rep = fromCheckpoint(t, pool, auditchain.ActionLog, false)
	if _, ok := problemOf(rep, auditchain.KindCheckpointMismatch); !ok {
		t.Fatalf("rewritten head link at the checkpoint not reported: %+v", rep.Problems)
	}
}

// Nil/empty: an empty chain checkpoints at 0 and verifies OK from there;
// its first link is then verified incrementally from genesis.
func TestEmptyChainCheckpointsAtZero(t *testing.T) {
	pool := freshDB(t, "chain_cp_empty")
	spec := auditchain.PGAuditEvents
	cp := checkpointAfterFull(t, pool, spec)
	if cp.Seq != 0 || cp.Hash != auditchain.Genesis {
		t.Fatalf("empty chain checkpoint = %+v, want seq 0 at genesis", cp)
	}
	// From checkpoint 0 the pass starts at genesis and reaches the head: it
	// covers the whole chain, so it is a full one.
	rep := fromCheckpoint(t, pool, spec, false)
	if !rep.OK() || rep.Mode != auditchain.ModeFull || rep.Links != 0 {
		t.Fatalf("empty chain from its checkpoint: %+v", rep)
	}
	exec(t, pool, `INSERT INTO sage.guard_pgaudit_events (database_name, logged_at,
		audit_type, correlated_by) VALUES ('orders', now(), 'SESSION', 'action')`)
	rep = fromCheckpoint(t, pool, spec, false)
	if !rep.OK() || rep.Links != 1 || rep.FromSeq != 1 || rep.RowsChecked != 1 {
		t.Fatalf("first link after an empty checkpoint: %+v", rep)
	}
}

// Without a checkpoint the verification is full, whatever was asked.
func TestNoCheckpointMeansFull(t *testing.T) {
	pool := freshDB(t, "chain_cp_none")
	insertAction(t, pool, "CREATE INDEX n1")
	if _, found, err := auditchain.LatestCheckpoint(context.Background(), pool,
		"action_log"); err != nil || found {
		t.Fatalf("fresh database has a checkpoint (found=%v, err=%v)", found, err)
	}
	rep := fromCheckpoint(t, pool, auditchain.ActionLog, false)
	if !rep.OK() || rep.Mode != auditchain.ModeFull || rep.FromSeq != 1 ||
		rep.CheckpointSeq != 0 {
		t.Fatalf("no checkpoint: %+v, want a full pass", rep)
	}
}

func problemOf(rep auditchain.Report, kind string) (auditchain.Problem, bool) {
	for _, p := range rep.Problems {
		if p.Kind == kind {
			return p, true
		}
	}
	return auditchain.Problem{}, false
}
