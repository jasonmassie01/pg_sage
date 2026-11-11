package auditchain_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// signHead verifies action_log in full, stores its checkpoint and appends
// a signed checkpoint link for it, returning that link's position.
func signHead(t *testing.T, pool *pgxpool.Pool, s auditchain.CheckpointSigner) int64 {
	t.Helper()
	ctx := context.Background()
	key := s.Trusted()
	rep, err := auditchain.VerifyFromCheckpointKeyed(ctx, pool, auditchain.ActionLog, true,
		&key, true)
	if err != nil || !rep.OK() {
		t.Fatalf("full verify before signing: %+v, %v", rep.Problems, err)
	}
	if err := auditchain.StoreCheckpoint(ctx, pool, rep); err != nil {
		t.Fatalf("store checkpoint: %v", err)
	}
	seq, appended, err := auditchain.AppendSignedCheckpoint(ctx, pool, rep, s)
	if err != nil || !appended || seq != rep.HeadSeq+1 {
		t.Fatalf("append signed checkpoint of %d: seq %d appended %v err %v", rep.HeadSeq,
			seq, appended, err)
	}
	return seq
}

func deleteAction(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	exec(t, pool, "DELETE FROM sage.action_log WHERE id = $1", id)
}

func prune(t *testing.T, pool *pgxpool.Pool, s auditchain.CheckpointSigner,
	exported int64) (auditchain.PruneResult, error) {
	t.Helper()
	return auditchain.PruneBelowSignedCheckpoint(context.Background(), pool,
		auditchain.ActionLog, auditchain.PruneRequest{Key: s.Trusted(),
			ExportedThrough: exported})
}

func verifyKeyed(t *testing.T, pool *pgxpool.Pool, s auditchain.CheckpointSigner,
	full bool) auditchain.Report {
	t.Helper()
	key := s.Trusted()
	rep, err := auditchain.VerifyFromCheckpointKeyed(context.Background(), pool,
		auditchain.ActionLog, full, &key, true)
	if err != nil {
		t.Fatalf("verify (full=%v): %v", full, err)
	}
	return rep
}

func opCount(t *testing.T, pool *pgxpool.Pool, op string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.audit_chain_link
		WHERE chain = 'action_log' AND op = $1`, op).Scan(&n)
	if err != nil {
		t.Fatalf("count %s links: %v", op, err)
	}
	return n
}

// prunableChain writes six actions and deletes the first three (links
// 1-6 inserts, 7-9 deletes), then signs head 9 (checkpoint link 10).
func prunableChain(t *testing.T, label string) (*pgxpool.Pool, auditchain.CheckpointSigner,
	int64) {
	t.Helper()
	pool := freshDB(t, label)
	var ids []int64
	for i := 0; i < 6; i++ {
		ids = append(ids, insertAction(t, pool, "CREATE INDEX p"+string(rune('a'+i))))
	}
	for _, id := range ids[:3] {
		deleteAction(t, pool, id)
	}
	s := testSigner(t, "k1")
	cpLink := signHead(t, pool, s)
	if cpLink != 10 {
		t.Fatalf("checkpoint link at %d, want 10", cpLink)
	}
	return pool, s, cpLink
}

func TestCheckpointPruneRefusedWhenUnexported(t *testing.T) {
	pool, s, cpLink := prunableChain(t, "chain_prune_unexported")
	for _, exported := range []int64{0, cpLink - 1} {
		_, err := prune(t, pool, s, exported)
		if !errors.Is(err, auditchain.ErrCheckpointNotExported) {
			t.Fatalf("prune with export at %d: %v, want ErrCheckpointNotExported", exported,
				err)
		}
		if n := linkCount(t, pool, "action_log"); n != 10 {
			t.Fatalf("a refused prune left %d links, want 10", n)
		}
	}
	other := testSigner(t, "k2")
	if _, err := prune(t, pool, other, cpLink); !errors.Is(err,
		auditchain.ErrCheckpointSignature) {
		t.Fatalf("prune with another key: %v, want ErrCheckpointSignature", err)
	}
	if n := linkCount(t, pool, "action_log"); n != 10 {
		t.Fatalf("a prune under another key left %d links, want 10", n)
	}
	res, err := prune(t, pool, s, cpLink)
	if err != nil || res.Pruned != 3 || res.Floor != 4 || res.Checkpoint != 9 ||
		res.CheckpointLink != cpLink || res.PruneLink != 11 {
		t.Fatalf("prune once exported: %+v, %v", res, err)
	}
}

func TestCheckpointPruneWithoutSignedCheckpointRefused(t *testing.T) {
	pool := freshDB(t, "chain_prune_unsigned")
	s := testSigner(t, "k1")
	if _, err := prune(t, pool, s, 100); !errors.Is(err, auditchain.ErrNoSignedCheckpoint) {
		t.Fatalf("prune of an empty chain: %v, want ErrNoSignedCheckpoint", err)
	}
	id := insertAction(t, pool, "CREATE INDEX u1")
	insertAction(t, pool, "CREATE INDEX u2")
	deleteAction(t, pool, id)
	checkpointAfterFull(t, pool, auditchain.ActionLog) // stored, never signed
	if _, err := prune(t, pool, s, 100); !errors.Is(err, auditchain.ErrNoSignedCheckpoint) {
		t.Fatalf("prune with only unsigned checkpoints: %v, want ErrNoSignedCheckpoint", err)
	}
	if n := linkCount(t, pool, "action_log"); n != 3 {
		t.Fatalf("links after refused prunes = %d, want 3", n)
	}
}

func TestCheckpointPruneThenVerifyReportsFromCheckpoint(t *testing.T) {
	pool, s, cpLink := prunableChain(t, "chain_prune_verify")
	if _, err := prune(t, pool, s, cpLink); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := linkCount(t, pool, "action_log"); n != 8 {
		t.Fatalf("links after the prune = %d, want 8 (4-11)", n)
	}
	if opCount(t, pool, auditchain.OpPrune) != 1 {
		t.Fatalf("want one prune link")
	}
	rep := verifyKeyed(t, pool, s, true)
	if !rep.OK() || rep.Mode != auditchain.ModeFull || rep.FromSeq != 4 || rep.ToSeq != 11 ||
		rep.FromCheckpoint != 9 || rep.PrunedBelow != 4 || rep.Links != 8 ||
		rep.Deleted != 3 || rep.RowsChecked != 3 || rep.SignedCheckpoints != 1 {
		t.Fatalf("full verify after the prune: %+v", rep)
	}
	inc := verifyKeyed(t, pool, s, false)
	if !inc.OK() || inc.Mode != auditchain.ModeIncremental || inc.FromSeq != 10 {
		t.Fatalf("incremental verify after the prune: %+v", inc)
	}
	key := s.Trusted()
	win := verify(t, pool, auditchain.ActionLog, auditchain.Window{FromSeq: 1, ToSeq: 6,
		Key: &key, Pruning: true})
	if !win.OK() || win.FromSeq != 4 || win.ToSeq != 6 || win.PrunedBelow != 4 {
		t.Fatalf("window from 1 after the prune: %+v", win)
	}
	insertAction(t, pool, "CREATE INDEX after_prune")
	if rep := verifyKeyed(t, pool, s, true); !rep.OK() || rep.HeadSeq != 12 {
		t.Fatalf("full verify after a later write: %+v", rep)
	}
}

// Fail closed: a pruned chain verified without the key that signed its
// anchor cannot vouch for the links it starts from.
func TestCheckpointPruneVerifyNeedsTrustedKey(t *testing.T) {
	pool, s, cpLink := prunableChain(t, "chain_prune_nokey")
	if _, err := prune(t, pool, s, cpLink); err != nil {
		t.Fatalf("prune: %v", err)
	}
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{Pruning: true})
	if rep.OK() || !hasKind(rep, auditchain.KindSignedCheckpoint) || rep.PrunedBelow != 4 {
		t.Fatalf("pruned chain verified without a key: %+v", rep)
	}
	other := testSigner(t, "k2").Trusted()
	rep = verify(t, pool, auditchain.ActionLog, auditchain.Window{Key: &other, Pruning: true})
	if rep.OK() || !hasKind(rep, auditchain.KindSignedCheckpoint) {
		t.Fatalf("pruned chain verified under another key: %+v", rep)
	}
}

// audit-01's anchor still holds after a prune: removing, rewriting or
// forging links at or below the signed checkpoint is reported.
func TestCheckpointPruneRegressionStillDetected(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		kinds []string
	}{
		{"tail cut below the checkpoint", `DELETE FROM sage.audit_chain_link
			WHERE chain = 'action_log' AND seq >= 6`, []string{
			auditchain.KindCheckpointMismatch}},
		{"floor link removed", `DELETE FROM sage.audit_chain_link
			WHERE chain = 'action_log' AND seq = 4`, []string{auditchain.KindGap}},
		{"link below the checkpoint rewritten", `UPDATE sage.audit_chain_link
			SET sealed_hash = 'x' WHERE chain = 'action_log' AND seq = 5`, []string{
			auditchain.KindHashMismatch}},
		{"checkpoint signature forged", `UPDATE sage.audit_chain_link
			SET state = replace(state, '"signature":"', '"signature":"00')
			WHERE chain = 'action_log' AND op = 'C'`, []string{auditchain.KindHashMismatch,
			auditchain.KindSignedCheckpoint}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, s, cpLink := prunableChain(t, "chain_prune_regress_"+string(rune('a'+i)))
			if _, err := prune(t, pool, s, cpLink); err != nil {
				t.Fatalf("prune: %v", err)
			}
			exec(t, pool, c.sql)
			rep := verifyKeyed(t, pool, s, true)
			if rep.OK() {
				t.Fatalf("verify after %s: OK, want problems", c.name)
			}
			for _, k := range c.kinds {
				if !hasKind(rep, k) {
					t.Errorf("verify after %s: no %s in %+v", c.name, k, rep.Problems)
				}
			}
		})
	}
}

// Links of a live row are never pruned: the floor stops at its first link.
func TestCheckpointPruneKeepsLiveRowsLinks(t *testing.T) {
	pool := freshDB(t, "chain_prune_live")
	a1 := insertAction(t, pool, "CREATE INDEX l1")
	a2 := insertAction(t, pool, "CREATE INDEX l2")
	insertAction(t, pool, "CREATE INDEX l3")
	deleteAction(t, pool, a2)
	s := testSigner(t, "k1")
	cpLink := signHead(t, pool, s)
	res, err := prune(t, pool, s, cpLink)
	if err != nil || res.Pruned != 0 || res.Floor != 1 || res.PruneLink != 0 {
		t.Fatalf("prune with live row 1: %+v, %v", res, err)
	}
	if opCount(t, pool, auditchain.OpPrune) != 0 || linkCount(t, pool, "action_log") != 5 {
		t.Fatalf("a prune with nothing to remove wrote links")
	}
	deleteAction(t, pool, a1)
	cpLink = signHead(t, pool, s)
	res, err = prune(t, pool, s, cpLink)
	if err != nil || res.Pruned != 2 || res.Floor != 3 || res.Checkpoint != 6 {
		t.Fatalf("prune after row 1 is deleted: %+v, %v", res, err)
	}
	rep := verifyKeyed(t, pool, s, true)
	if !rep.OK() || rep.FromSeq != 3 || rep.Deleted != 2 || rep.RowsChecked != 1 ||
		rep.SignedCheckpoints != 2 {
		t.Fatalf("full verify after the second prune: %+v", rep)
	}
}

// A row that vanished without a delete link is evidence: the prune that
// would erase its links is refused.
func TestCheckpointPruneRefusesVanishedRow(t *testing.T) {
	pool := freshDB(t, "chain_prune_vanished")
	a1 := insertAction(t, pool, "CREATE INDEX v1")
	a2 := insertAction(t, pool, "CREATE INDEX v2")
	insertAction(t, pool, "CREATE INDEX v3")
	deleteAction(t, pool, a2)
	s := testSigner(t, "k1")
	cpLink := signHead(t, pool, s)
	bypass(t, pool, "DELETE FROM sage.action_log WHERE id = $1", a1)
	_, err := prune(t, pool, s, cpLink)
	if !errors.Is(err, auditchain.ErrPruneUnsafe) ||
		!strings.Contains(err.Error(), "row "+itoa(a1)) {
		t.Fatalf("prune over a vanished row: %v, want ErrPruneUnsafe naming row %d", err, a1)
	}
	if n := linkCount(t, pool, "action_log"); n != 5 {
		t.Fatalf("a refused prune left %d links, want 5", n)
	}
}

// Writers keep appending while two prunes race: one prunes, the other has
// nothing left to do, and the chain verifies.
func TestCheckpointPruneConcurrentWriters(t *testing.T) {
	pool, s, cpLink := prunableChain(t, "chain_prune_race")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				_, err := pool.Exec(context.Background(), `INSERT INTO sage.action_log
					(action_type, sql_executed, outcome) VALUES ('x', $1, 'pending')`,
					"CREATE INDEX w"+itoa(int64(w*100+i)))
				if err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := prune(t, pool, s, cpLink); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write or prune: %v", err)
	}
	if n := opCount(t, pool, auditchain.OpPrune); n != 1 {
		t.Fatalf("%d prune links after two racing prunes, want 1", n)
	}
	rep := verifyKeyed(t, pool, s, true)
	if !rep.OK() || rep.PrunedBelow != 4 || rep.RowsChecked != 63 {
		t.Fatalf("full verify after racing writers and prunes: %+v", rep)
	}
}

// State: a pruned chain is signed again (its head is the prune link) and
// pruned again from the newer checkpoint.
func TestCheckpointPruneTwice(t *testing.T) {
	pool, s, cpLink := prunableChain(t, "chain_prune_twice")
	if _, err := prune(t, pool, s, cpLink); err != nil {
		t.Fatalf("first prune: %v", err)
	}
	var ids []int64
	rows, err := pool.Query(context.Background(),
		"SELECT id FROM sage.action_log ORDER BY id")
	if err != nil {
		t.Fatalf("list actions: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		deleteAction(t, pool, id) // links 12-14
	}
	insertAction(t, pool, "CREATE INDEX keep") // link 15
	cp2 := signHead(t, pool, s)                // link 16, signing 15
	res, err := prune(t, pool, s, cp2)
	if err != nil || res.Floor != 15 || res.Pruned != 11 || res.Checkpoint != 15 {
		t.Fatalf("second prune: %+v, %v", res, err)
	}
	rep := verifyKeyed(t, pool, s, true)
	if !rep.OK() || rep.FromSeq != 15 || rep.FromCheckpoint != 15 || rep.RowsChecked != 1 {
		t.Fatalf("full verify after the second prune: %+v", rep)
	}
}

// Fail closed: with pruning off, a prune link proves nothing. Anyone able
// to append a link could otherwise write one and erase signed history; a
// full pass then reports the removed links missing. Verifications that
// start above the floor are unaffected.
func TestCheckpointPruneLinkIgnoredWhenPruningOff(t *testing.T) {
	pool, s, cpLink := prunableChain(t, "chain_prune_off")
	if _, err := prune(t, pool, s, cpLink); err != nil {
		t.Fatalf("prune: %v", err)
	}
	key := s.Trusted()
	rep, err := auditchain.VerifyFromCheckpointKeyed(context.Background(), pool,
		auditchain.ActionLog, true, &key, false)
	if err != nil {
		t.Fatalf("full verify with pruning off: %v", err)
	}
	if rep.OK() || rep.FromSeq != 1 || rep.PrunedBelow != 0 || rep.FromCheckpoint != 0 ||
		!hasKind(rep, auditchain.KindGap) || !hasKind(rep, auditchain.KindSignedCheckpoint) {
		t.Fatalf("full verify of a pruned chain with pruning off: %+v", rep)
	}
	for _, p := range rep.Problems {
		if p.Kind == auditchain.KindGap && p.Seq != 1 {
			t.Errorf("gap at %d, want the removed links 1-3 reported from 1: %+v", p.Seq, p)
		}
		if p.Kind == auditchain.KindSignedCheckpoint && p.Seq != 11 {
			t.Errorf("signed_checkpoint at %d, want the prune link 11: %+v", p.Seq, p)
		}
	}
	win := verify(t, pool, auditchain.ActionLog, auditchain.Window{FromSeq: 5, ToSeq: 6,
		Key: &key})
	if !win.OK() || win.FromSeq != 5 || win.PrunedBelow != 0 {
		t.Fatalf("window above the floor with pruning off: %+v", win)
	}
}
