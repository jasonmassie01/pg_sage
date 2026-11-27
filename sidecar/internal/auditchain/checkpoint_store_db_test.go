package auditchain_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

func checkpointRows(t *testing.T, pool *pgxpool.Pool, chain string) (int, int) {
	t.Helper()
	var all, full int
	err := pool.QueryRow(context.Background(), `SELECT count(*),
		count(*) FILTER (WHERE mode = 'full') FROM sage.audit_chain_checkpoint
		WHERE chain = $1`, chain).Scan(&all, &full)
	if err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	return all, full
}

func okReport(chain string, seq int64, mode string) auditchain.Report {
	return auditchain.Report{Chain: chain, FromSeq: 1, ToSeq: seq, HeadSeq: seq,
		HeadHash: fmt.Sprintf("%064d", seq), Mode: mode, Problems: []auditchain.Problem{}}
}

// The table keeps the newest CheckpointsKept checkpoints per chain plus
// the newest full one, so it never grows with the chain; other chains'
// checkpoints are untouched.
func TestCheckpointsAreBounded(t *testing.T) {
	pool := freshDB(t, "chain_cp_bounded")
	ctx := context.Background()
	store := func(rep auditchain.Report) {
		t.Helper()
		if err := auditchain.StoreCheckpoint(ctx, pool, rep); err != nil {
			t.Fatalf("store checkpoint %d: %v", rep.HeadSeq, err)
		}
	}
	store(okReport("auth_audit", 7, auditchain.ModeFull))
	store(okReport("action_log", 1, auditchain.ModeFull))
	extra := auditchain.CheckpointsKept + 5
	for seq := int64(2); seq <= int64(extra)+1; seq++ {
		store(okReport("action_log", seq, auditchain.ModeIncremental))
	}
	all, full := checkpointRows(t, pool, "action_log")
	if all != auditchain.CheckpointsKept+1 || full != 1 {
		t.Fatalf("after %d checkpoints: %d kept (%d full), want %d with the full one",
			extra+1, all, full, auditchain.CheckpointsKept+1)
	}
	if n, _ := checkpointRows(t, pool, "auth_audit"); n != 1 {
		t.Fatalf("pruning action_log removed auth_audit's checkpoint (%d left)", n)
	}
	cp, found, err := auditchain.LatestCheckpoint(ctx, pool, "action_log")
	if err != nil || !found || cp.Seq != int64(extra)+1 ||
		cp.Mode != auditchain.ModeIncremental || cp.Hash != fmt.Sprintf("%064d", cp.Seq) {
		t.Fatalf("latest checkpoint = %+v (found=%v, err=%v)", cp, found, err)
	}
}

// State transitions of the full-pass schedule: no full checkpoint is due;
// a fresh one is not; one older than the interval is; 0 hours always is.
// An incremental checkpoint at the same position never hides the full one.
func TestFullVerifyDue(t *testing.T) {
	pool := freshDB(t, "chain_cp_due")
	ctx := context.Background()
	due := func(hours int) bool {
		t.Helper()
		d, err := auditchain.FullVerifyDue(ctx, pool, "action_log", hours)
		if err != nil {
			t.Fatalf("full verify due (%dh): %v", hours, err)
		}
		return d
	}
	if !due(168) {
		t.Fatalf("no checkpoint: a full pass must be due")
	}
	if err := auditchain.StoreCheckpoint(ctx, pool,
		okReport("action_log", 3, auditchain.ModeIncremental)); err != nil {
		t.Fatalf("store incremental: %v", err)
	}
	if !due(168) {
		t.Fatalf("only an incremental checkpoint: a full pass must be due")
	}
	for _, mode := range []string{auditchain.ModeFull, auditchain.ModeIncremental} {
		if err := auditchain.StoreCheckpoint(ctx, pool,
			okReport("action_log", 4, mode)); err != nil {
			t.Fatalf("store %s: %v", mode, err)
		}
	}
	if due(168) || !due(0) {
		t.Fatalf("fresh full checkpoint: due(168)=%v due(0)=%v, want false and true",
			due(168), due(0))
	}
	exec(t, pool, `UPDATE sage.audit_chain_checkpoint
		SET verified_at = now() - interval '169 hours' WHERE mode = 'full'`)
	if !due(168) || due(170) {
		t.Fatalf("full checkpoint 169h old: due(168)=%v due(170)=%v", due(168), due(170))
	}
	if _, err := auditchain.FullVerifyDue(ctx, pool, "action_log", -1); err == nil {
		t.Fatalf("negative interval accepted")
	}
}

// Invalid input: only a clean report that reached the head checkpoints;
// a failing, truncated, windowed or unnamed one is refused, and nothing
// is stored.
func TestStoreCheckpointRefusesUnprovenReports(t *testing.T) {
	pool := freshDB(t, "chain_cp_refuse")
	ctx := context.Background()
	failing := okReport("action_log", 5, auditchain.ModeFull)
	failing.Problems = []auditchain.Problem{{Kind: auditchain.KindGap, Seq: 2}}
	truncated := okReport("action_log", 5, auditchain.ModeFull)
	truncated.ProblemsTruncated = true
	window := okReport("action_log", 5, auditchain.ModeWindow)
	window.ToSeq = 3
	unnamed := okReport("", 5, auditchain.ModeFull)
	for name, rep := range map[string]auditchain.Report{"failing": failing,
		"truncated": truncated, "window": window, "unnamed": unnamed} {
		err := auditchain.StoreCheckpoint(ctx, pool, rep)
		if err == nil || !strings.Contains(err.Error(), "checkpoint") {
			t.Fatalf("%s report: err = %v, want a refusal naming the checkpoint", name, err)
		}
	}
	if n, _ := checkpointRows(t, pool, "action_log"); n != 0 {
		t.Fatalf("refused reports stored %d checkpoints", n)
	}
	if err := auditchain.StoreCheckpoint(ctx, nil,
		okReport("action_log", 5, auditchain.ModeFull)); err == nil {
		t.Fatalf("nil database accepted")
	}
}

// Error propagation: a dead connection surfaces as an error naming the
// chain from every checkpoint call, never as "no checkpoint".
func TestCheckpointCallsPropagateErrors(t *testing.T) {
	pool := freshDB(t, "chain_cp_err")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := auditchain.LatestCheckpoint(ctx, pool, "action_log"); err == nil ||
		!strings.Contains(err.Error(), "action_log") {
		t.Fatalf("latest checkpoint on a cancelled context: err = %v", err)
	}
	if _, err := auditchain.FullVerifyDue(ctx, pool, "action_log", 1); err == nil ||
		!strings.Contains(err.Error(), "action_log") {
		t.Fatalf("full verify due on a cancelled context: err = %v", err)
	}
	if _, err := auditchain.VerifyFromCheckpoint(ctx, pool, auditchain.ActionLog,
		false); err == nil || !strings.Contains(err.Error(), "action_log") {
		t.Fatalf("verify from checkpoint on a cancelled context: err = %v", err)
	}
}

// Nil/empty: without the checkpoint table (a database bootstrapped by an
// older sidecar), every pass is full, as before checkpoints existed; the
// chain is still verified rather than reported unverifiable.
func TestMissingCheckpointTableMeansFull(t *testing.T) {
	pool := freshDB(t, "chain_cp_no_table")
	ctx := context.Background()
	insertAction(t, pool, "CREATE INDEX nocp1")
	exec(t, pool, "DROP TABLE sage.audit_chain_checkpoint")
	due, err := auditchain.FullVerifyDue(ctx, pool, "action_log", 168)
	if err != nil || !due {
		t.Fatalf("full verify due without the table: due=%v err=%v, want true", due, err)
	}
	rep, err := auditchain.VerifyFromCheckpoint(ctx, pool, auditchain.ActionLog, false)
	if err != nil || !rep.OK() || rep.Mode != auditchain.ModeFull || rep.Links != 1 {
		t.Fatalf("verify without the table: %+v err %v, want a clean full pass", rep, err)
	}
}
