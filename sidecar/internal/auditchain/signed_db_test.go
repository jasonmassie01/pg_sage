package auditchain_test

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// A signed checkpoint is a link of its own chain: appended once per new
// head, verified by a full pass under its key, and skipped under a key it
// was not signed with.
func TestSignedCheckpointLinkLifecycle(t *testing.T) {
	pool := freshDB(t, "chain_signed_link")
	ctx := context.Background()
	s := testSigner(t, "k1")
	empty := verifyKeyed(t, pool, s, true)
	if seq, appended, err := auditchain.AppendSignedCheckpoint(ctx, pool, empty,
		s); err != nil || appended || seq != 0 {
		t.Fatalf("sign an empty chain: seq %d appended %v err %v", seq, appended, err)
	}
	insertAction(t, pool, "CREATE INDEX s1")
	insertAction(t, pool, "CREATE INDEX s2")
	if got := signHead(t, pool, s); got != 3 {
		t.Fatalf("checkpoint link at %d, want 3", got)
	}
	rep := verifyKeyed(t, pool, s, true)
	if !rep.OK() || rep.SignedCheckpoints != 1 || rep.Links != 3 || rep.RowsChecked != 2 {
		t.Fatalf("full verify with a checkpoint link: %+v", rep)
	}
	// Nothing new since the last signature: no second link.
	if seq, appended, err := auditchain.AppendSignedCheckpoint(ctx, pool, rep,
		s); err != nil || appended || seq != 3 {
		t.Fatalf("re-sign an unchanged head: seq %d appended %v err %v", seq, appended, err)
	}
	if n := opCount(t, pool, auditchain.OpCheckpoint); n != 1 {
		t.Fatalf("%d checkpoint links, want 1", n)
	}
	insertAction(t, pool, "CREATE INDEX s3")
	if got := signHead(t, pool, s); got != 5 {
		t.Fatalf("second checkpoint link at %d, want 5", got)
	}
	if rep := verifyKeyed(t, pool, s, true); !rep.OK() || rep.SignedCheckpoints != 2 {
		t.Fatalf("full verify with two checkpoint links: %+v", rep)
	}
	inc := verifyKeyed(t, pool, s, false)
	if !inc.OK() || inc.Mode != auditchain.ModeIncremental {
		t.Fatalf("incremental verify over a checkpoint link: %+v", inc)
	}
}

// A checkpoint link signed by another key id is skipped (a rotated key);
// another key under the signer's id fails; no key checks nothing.
func TestSignedCheckpointLinkUnderOtherKeys(t *testing.T) {
	pool := freshDB(t, "chain_signed_other")
	s := testSigner(t, "k1")
	insertAction(t, pool, "CREATE INDEX o1")
	signHead(t, pool, s)
	other := testSigner(t, "k2").Trusted()
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{Key: &other})
	if !rep.OK() || rep.SignedCheckpoints != 0 {
		t.Fatalf("verify under a key the checkpoints were not signed with: %+v", rep)
	}
	impostor := auditchain.TrustedKey{ID: "k1", Public: other.Public}
	rep = verify(t, pool, auditchain.ActionLog, auditchain.Window{Key: &impostor})
	if rep.OK() || !hasKind(rep, auditchain.KindSignedCheckpoint) {
		t.Fatalf("verify with another key under the signer's id: %+v", rep)
	}
	if rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{}); !rep.OK() {
		t.Fatalf("an unpruned chain verified without a key: %+v", rep.Problems)
	}
}

// The checkpoint link names the head it signs: rewriting that head link
// is reported on the checkpoint too.
func TestSignedCheckpointLinkDetectsRewrittenHead(t *testing.T) {
	pool := freshDB(t, "chain_signed_head")
	insertAction(t, pool, "CREATE INDEX h1")
	insertAction(t, pool, "CREATE INDEX h2")
	s := testSigner(t, "k1")
	signHead(t, pool, s)
	exec(t, pool, `UPDATE sage.audit_chain_link SET hash = repeat('0', 64)
		WHERE chain = 'action_log' AND seq = 2`)
	rep := verifyKeyed(t, pool, s, true)
	if !hasKind(rep, auditchain.KindSignedCheckpoint) || !hasKind(rep,
		auditchain.KindHashMismatch) {
		t.Fatalf("full verify after the signed head was rewritten: %+v", rep.Problems)
	}
}

func TestAppendSignedCheckpointRefusesUncleanReports(t *testing.T) {
	pool := freshDB(t, "chain_signed_refuse")
	ctx := context.Background()
	s := testSigner(t, "k1")
	insertAction(t, pool, "CREATE INDEX r1")
	bad := auditchain.Report{Chain: "action_log", Mode: auditchain.ModeFull, HeadSeq: 1,
		ToSeq: 1, HeadHash: headHash, Problems: []auditchain.Problem{{Kind: "gap"}}}
	if _, _, err := auditchain.AppendSignedCheckpoint(ctx, pool, bad, s); err == nil {
		t.Fatal("signed a report with problems")
	}
	win := auditchain.Report{Chain: "action_log", Mode: auditchain.ModeWindow, HeadSeq: 1,
		ToSeq: 1, HeadHash: headHash}
	if _, _, err := auditchain.AppendSignedCheckpoint(ctx, pool, win, s); err == nil {
		t.Fatal("signed a window report")
	}
	if _, _, err := auditchain.AppendSignedCheckpoint(ctx, nil, win, s); err == nil {
		t.Fatal("signed without a database")
	}
	stale := verifyKeyed(t, pool, s, true)
	exec(t, pool, `UPDATE sage.audit_chain_link SET hash = repeat('1', 64)
		WHERE chain = 'action_log' AND seq = 1`)
	if _, _, err := auditchain.AppendSignedCheckpoint(ctx, pool, stale, s); err == nil {
		t.Fatal("signed a head whose link changed after it was verified")
	}
	if n := opCount(t, pool, auditchain.OpCheckpoint); n != 0 {
		t.Fatalf("%d checkpoint links after refusals, want 0", n)
	}
}
