package modelgate

import (
	"strings"
	"testing"
	"time"
)

// A model change in the very tick a grant was approved in still revokes
// with its own audit row: the gate's history and the digest say revoked,
// never a stale granted.
func TestGateDB_ModelChangeInTheGrantsTickRecordsTheRevocation(t *testing.T) {
	pool, ctx := livePool(t)
	seedReviews(t, ctx, pool, mix{n: 50, genImproved: 45, modelImproved: 45,
		modelOnly: true, model: "m1"})
	g := newGate(pool)
	evaluate(t, ctx, g, "m1", tick0)
	pending, err := g.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	if _, err := g.Approve(ctx, pending[0].ID, "admin"); err != nil {
		t.Fatal(err)
	}
	res := evaluate(t, ctx, g, "m2", tick0)
	if len(res.Revoked) != 1 || res.Revoked[0] != "m1" {
		t.Fatalf("model change: %+v, want m1 revoked", res)
	}
	if authority(t, ctx, pool, "m1") {
		t.Fatal("the model change left m1's authority live")
	}
	m1, err := latest(ctx, pool, "m1", RuleM1)
	if err != nil || m1 == nil || m1.Verdict != StateRevoked || m1.RevokedAt == nil ||
		!strings.Contains(m1.Reason, `now "m2"`) {
		t.Fatalf("m1's latest G-M1 row = %+v %v, want the revocation recorded", m1, err)
	}
	notes, err := g.Notes(ctx, tick0.Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(notes) != 2 || !strings.Contains(notes[1], "revoked for model m1") {
		t.Fatalf("digest notes = %q %v, want the grant then the revocation", notes, err)
	}
	// A second evaluation of the same tick neither revokes again nor adds a row.
	if res := evaluate(t, ctx, g, "m2", tick0); len(res.Revoked) != 0 {
		t.Fatalf("a repeated evaluation revoked again: %+v", res)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_gate
		WHERE model = 'm1' AND verdict = 'revoked'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("m1's revoked rows = %d %v, want 1", n, err)
	}
	assertNoGrantWithoutApproval(t, pool)
}
