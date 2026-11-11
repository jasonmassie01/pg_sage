package auditjob

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

func jobSigner(t *testing.T) *auditchain.CheckpointSigner {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &auditchain.CheckpointSigner{ID: "job-key", Key: key}
}

func markerLinks(t *testing.T, pool *pgxpool.Pool, op string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.audit_chain_link WHERE chain = 'action_log' AND op = $1`,
		op).Scan(&n); err != nil {
		t.Fatalf("count %s links: %v", op, err)
	}
	return n
}

func actionLinks(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.audit_chain_link WHERE chain = 'action_log'`).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// threeActionsTwoDeleted writes links 1-3 (inserts) and 4-5 (deletes of
// the first two rows).
func threeActionsTwoDeleted(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	addActions(t, pool, 3)
	mustExec(t, pool, `DELETE FROM sage.action_log WHERE id IN
		(SELECT id FROM sage.action_log ORDER BY id LIMIT 2)`)
}

// State transitions: unsigned by default; signed once per new head with a
// signer; pruned only once opted in, exported and on a full pass; signed
// again after the prune and verified from the signed checkpoint.
func TestSignedCheckpointsAndOptInPrune(t *testing.T) {
	pool := fresh(t, "auditjob_signed_prune", true)
	threeActionsTwoDeleted(t, pool)
	plain := Source{Name: "orders", Pool: pool}
	if rep := runMode(t, plain, "action_log"); !rep.OK() ||
		markerLinks(t, pool, auditchain.OpCheckpoint) != 0 {
		t.Fatalf("a run without a signer: %+v, want no checkpoint link", rep)
	}
	fc := &fakeCursors{}
	src := Source{Name: "orders", Pool: pool, Cursors: fc, Signer: jobSigner(t)}
	if rep := runMode(t, src, "action_log"); !rep.OK() || rep.HeadSeq != 5 {
		t.Fatalf("first signed run: %+v", rep)
	}
	if markerLinks(t, pool, auditchain.OpCheckpoint) != 1 || actionLinks(t, pool) != 6 {
		t.Fatalf("first signed run wrote %d checkpoint links, %d links in all",
			markerLinks(t, pool, auditchain.OpCheckpoint), actionLinks(t, pool))
	}
	fc.set("action_log", 6, linkHashAt(t, pool, 6))
	runMode(t, src, "action_log")
	if actionLinks(t, pool) != 6 {
		t.Fatalf("an unchanged head or a run without the prune switch wrote links: %d",
			actionLinks(t, pool))
	}
	src.PruneBelowSignedCheckpoint = true
	runMode(t, src, "action_log")
	if markerLinks(t, pool, auditchain.OpPrune) != 1 || actionLinks(t, pool) != 5 {
		t.Fatalf("opted-in run: %d prune links, %d links, want 1 and 5 (3-7)",
			markerLinks(t, pool, auditchain.OpPrune), actionLinks(t, pool))
	}
	rep := runMode(t, src, "action_log")
	if !rep.OK() || rep.PrunedBelow != 3 || rep.FromCheckpoint != 5 || rep.FromSeq != 3 {
		t.Fatalf("run after the prune: %+v", rep)
	}
	if n, _, _ := openFindings(t, pool); n != 0 {
		t.Fatalf("%d open findings after the prune, want 0", n)
	}
	if markerLinks(t, pool, auditchain.OpCheckpoint) != 2 {
		t.Fatalf("the prune link was not signed: %d checkpoint links",
			markerLinks(t, pool, auditchain.OpCheckpoint))
	}
}

// Boundary: the prune waits for a full pass, an export cursor past the
// checkpoint link on every sink, and the switch; incremental runs and a
// lagging sink prune nothing.
func TestPruneWaitsForFullPassAndExport(t *testing.T) {
	pool := fresh(t, "auditjob_prune_waits", true)
	threeActionsTwoDeleted(t, pool)
	src := Source{Name: "orders", Pool: pool, Signer: jobSigner(t),
		PruneBelowSignedCheckpoint: true, FullVerifyHours: 168}
	runMode(t, src, "action_log") // full, signs head 5 at link 6
	if actionLinks(t, pool) != 6 {
		t.Fatalf("pruned without any export cursor: %d links", actionLinks(t, pool))
	}
	fc := &fakeCursors{}
	src.Cursors = fc
	fc.set("action_log", 5, linkHashAt(t, pool, 5))
	if rep := runMode(t, src, "action_log"); rep.Mode != auditchain.ModeIncremental {
		t.Fatalf("second run mode %s, want incremental", rep.Mode)
	}
	if markerLinks(t, pool, auditchain.OpPrune) != 0 {
		t.Fatal("an incremental run pruned")
	}
	src.FullVerifyHours = 0
	runMode(t, src, "action_log")
	if markerLinks(t, pool, auditchain.OpPrune) != 0 {
		t.Fatal("pruned below a checkpoint link the sink has not received")
	}
	fc.set("action_log", 6, linkHashAt(t, pool, 6))
	runMode(t, src, "action_log")
	if markerLinks(t, pool, auditchain.OpPrune) != 1 {
		t.Fatal("no prune once the sink received the checkpoint link")
	}
}

// Error propagation: a cursor read that fails stops the prune and names
// the cause; the chain keeps its links.
func TestPruneCursorErrorIsReported(t *testing.T) {
	pool := fresh(t, "auditjob_prune_cursor_err", true)
	threeActionsTwoDeleted(t, pool)
	fc := &fakeCursors{err: errors.New("control database unreachable")}
	src := Source{Name: "orders", Pool: pool, Cursors: fc, Signer: jobSigner(t),
		PruneBelowSignedCheckpoint: true}
	_, err := Run(context.Background(), src)
	if err == nil || !strings.Contains(err.Error(), "control database unreachable") ||
		!strings.Contains(err.Error(), "prune") {
		t.Fatalf("run with failing cursors: %v, want the prune to name the cause", err)
	}
	if markerLinks(t, pool, auditchain.OpPrune) != 0 || actionLinks(t, pool) != 6 {
		t.Fatalf("links changed although the cursors could not be read")
	}
}
