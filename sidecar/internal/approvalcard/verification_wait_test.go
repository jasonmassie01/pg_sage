package approvalcard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// One change per object: a card whose object has a verification in
// flight says so, and that approving it overrides that verification. The
// wait changes as verdicts land, so it is read live and is not part of the
// content a decision is bound to.

func indexWait(id int64) policy.PendingVerification {
	return policy.PendingVerification{ActionID: id, Object: "table:public.memories",
		Until: now.Add(10 * time.Minute), HardDeadline: now.Add(73 * time.Hour)}
}

func memoriesCard() Inputs {
	a := queued(7, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY idx_memories_status_type_quality_current ON "+
			"public.memories (status, fact_type, quality_score) WHERE valid_to IS NULL",
		"DROP INDEX CONCURRENTLY IF EXISTS public.idx_memories_status_type_quality_current",
		"moderate")
	return Inputs{Database: "lifeos", Action: a, Now: now}
}

func TestCardShowsThePendingVerificationAndTheOverride(t *testing.T) {
	in := memoriesCard()
	in.Waits = []policy.PendingVerification{indexWait(6410)}
	c := Assemble(in)
	w := c.VerificationWait
	if w == nil || len(w.ActionIDs) != 1 || w.ActionIDs[0] != 6410 ||
		w.Objects[0] != "table:public.memories" || w.Until == nil ||
		!w.Until.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("wait %+v", w)
	}
	for _, want := range []string{"Awaiting verification of action 6410",
		"until 2026-10-03 12:10 UTC",
		"approving overrides pending verification of action 6410"} {
		if !strings.Contains(w.Line, want) {
			t.Fatalf("line %q lacks %q", w.Line, want)
		}
	}
	if !strings.Contains(Text(c, now), w.Line) {
		t.Fatalf("chat text lacks the wait:\n%s", Text(c, now))
	}
	if !hasCode(c, "awaiting_verification") {
		t.Fatalf("why %v, want awaiting_verification", reasonCodes(c))
	}
}

func TestCardWithoutWaitShowsNone(t *testing.T) {
	c := Assemble(memoriesCard())
	if c.VerificationWait != nil || hasCode(c, "awaiting_verification") ||
		strings.Contains(Text(c, now), "verification of action") {
		t.Fatalf("card without a wait shows one: %+v", c.VerificationWait)
	}
}

// A wait past its hard deadline has been released: the card does not ask
// the operator to override it.
func TestCardDropsAReleasedWait(t *testing.T) {
	in := memoriesCard()
	expired := indexWait(6400)
	expired.HardDeadline = now.Add(-time.Minute)
	in.Waits = []policy.PendingVerification{expired}
	if c := Assemble(in); c.VerificationWait != nil {
		t.Fatalf("released wait shown: %+v", c.VerificationWait)
	}
	in.Waits = append(in.Waits, indexWait(6410))
	c := Assemble(in)
	if c.VerificationWait == nil || len(c.VerificationWait.ActionIDs) != 1 ||
		c.VerificationWait.ActionIDs[0] != 6410 {
		t.Fatalf("wait %+v, want only action 6410", c.VerificationWait)
	}
}

func TestCardNamesEveryPendingChange(t *testing.T) {
	in := memoriesCard()
	held := policy.PendingVerification{DecisionID: 99, Object: "table:public.memories",
		Until: now.Add(5 * time.Minute), HardDeadline: now.Add(5 * time.Minute)}
	in.Waits = []policy.PendingVerification{indexWait(6410), held}
	w := Assemble(in).VerificationWait
	if w == nil || !strings.Contains(w.Line, "action 6410") ||
		!strings.Contains(w.Line, "decision 99") || len(w.Objects) != 1 {
		t.Fatalf("wait %+v", w)
	}
}

func TestCardWaitUnavailableIsShown(t *testing.T) {
	in := memoriesCard()
	in.WaitsErr = errors.New("relation sage.action_outcome does not exist")
	w := Assemble(in).VerificationWait
	if w == nil || !strings.Contains(w.Unavailable, "sage.action_outcome") ||
		!strings.Contains(w.Line, "unavailable") {
		t.Fatalf("wait %+v, want the unread state shown", w)
	}
}

func TestCardWaitIsNotPartOfTheContentHash(t *testing.T) {
	in := memoriesCard()
	plain := Assemble(in)
	in.Waits = []policy.PendingVerification{indexWait(6410)}
	if waiting := Assemble(in); waiting.CardHash != plain.CardHash {
		t.Fatal("the wait changed the card hash")
	}
}

type fakeWaits struct {
	sql     string
	targets []string
	got     []policy.PendingVerification
	err     error
}

func (f *fakeWaits) PendingFor(_ context.Context, sql string, targets []string) (
	[]policy.PendingVerification, error) {
	f.sql, f.targets = sql, targets
	return f.got, f.err
}

// The loader asks the wait source with the queued SQL and the finding's
// object.
func TestLoaderReadsTheWaitOfTheQueuedChange(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, _ := queueOptimizerIndex(t, ctx, pool)
	waits := &fakeWaits{got: []policy.PendingVerification{indexWait(6410)}}
	l := Loader{Pool: pool, Database: "orders", Waits: waits, Now: func() time.Time {
		return now
	}}
	c, err := l.Card(ctx, queueID)
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if c.VerificationWait == nil || c.VerificationWait.ActionIDs[0] != 6410 ||
		waits.sql != c.SQL || len(waits.targets) != 1 || waits.targets[0] !=
		c.Finding.Object {
		t.Fatalf("wait %+v, source asked sql=%q targets=%v", c.VerificationWait,
			waits.sql, waits.targets)
	}
	waits.got, waits.err = nil, errors.New("boom")
	c, err = l.Card(ctx, queueID)
	if err != nil || c.VerificationWait == nil || c.VerificationWait.Unavailable != "boom" {
		t.Fatalf("unreadable waits: card err %v, wait %+v", err, c.VerificationWait)
	}
}
