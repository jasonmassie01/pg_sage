package facts

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// facts-09: every Store on a pool tells the pool's proposal notifier about
// each new or reopened proposal, so a proposal from MCP, mark_object, Ask
// Sage, the worker or the first look sends exactly one fact card.

type notifierLog struct {
	mu  sync.Mutex
	ids []int64
}

func (n *notifierLog) fn(_ context.Context, f Fact) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ids = append(n.ids, f.ID)
}

func (n *notifierLog) got() []int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]int64(nil), n.ids...)
}

func listen(t *testing.T, pool *pgxpool.Pool) *notifierLog {
	t.Helper()
	n := &notifierLog{}
	t.Cleanup(SetProposalNotifier(pool, n.fn))
	return n
}

func fixtureProposal(subject string) Proposal {
	return Proposal{Type: TypeTestFixture, Kind: KindSchema, Subject: subject,
		Source: SourceDetector, Evidence: []Citation{citation("schemas:" + subject)}}
}

func TestProposalNotifierToldOncePerNewFact(t *testing.T) {
	pool, ctx := livePool(t)
	n := listen(t, pool)
	first, created, err := NewStore(pool).Propose(ctx, fixtureProposal("test_note_*"))
	if err != nil || !created {
		t.Fatalf("first proposal: created=%v err=%v", created, err)
	}
	if got := n.got(); len(got) != 1 || got[0] != first.ID {
		t.Fatalf("notified %v, want [%d]", got, first.ID)
	}
	// Another Store on the same pool shares the notifier; a repeat is not new.
	again, created, err := NewStore(pool).WithLog(nil).Propose(ctx,
		fixtureProposal("test_note_*"))
	if err != nil || created || again.ID != first.ID || again.Proposals != 2 {
		t.Fatalf("repeat: %+v created=%v err=%v", again, created, err)
	}
	if got := n.got(); len(got) != 1 {
		t.Fatalf("a repeated proposal was notified: %v", got)
	}
	other, _, err := NewStore(pool).Propose(ctx, fixtureProposal("test_other_*"))
	if err != nil {
		t.Fatal(err)
	}
	if got := n.got(); len(got) != 2 || got[1] != other.ID {
		t.Fatalf("notified %v, want [%d %d]", got, first.ID, other.ID)
	}
}

// State transitions: a rejected fact proposed again stays rejected (no
// card); an expired one is reopened for a decision (a card).
func TestProposalNotifierFollowsTheDecision(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	rejected, _, err := s.Propose(ctx, fixtureProposal("test_rej_*"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, rejected.ID, Decision{Actor: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	expired, _, err := s.Propose(ctx, fixtureProposal("test_exp_*"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Expire(ctx, expired.ID, "alice@example.com", "stale"); err != nil {
		t.Fatal(err)
	}
	n := listen(t, pool)
	if f, created, err := s.Propose(ctx, fixtureProposal("test_rej_*")); err != nil ||
		created || f.Status != StatusRejected {
		t.Fatalf("re-proposed rejection: %+v created=%v err=%v", f, created, err)
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("a rejected fact sent a card again: %v", got)
	}
	f, created, err := s.Propose(ctx, fixtureProposal("test_exp_*"))
	if err != nil || !created || f.Status != StatusProposed {
		t.Fatalf("reopened: %+v created=%v err=%v", f, created, err)
	}
	if got := n.got(); len(got) != 1 || got[0] != expired.ID {
		t.Fatalf("reopened fact notified %v, want [%d]", got, expired.ID)
	}
}

// Invalid input: a refused proposal records nothing and notifies nothing.
func TestProposalNotifierNotToldOfARefusedProposal(t *testing.T) {
	pool, ctx := livePool(t)
	n := listen(t, pool)
	for _, p := range []Proposal{fixtureProposal("sage"), fixtureProposal("not a schema"),
		{Type: TypeTestFixture, Kind: KindSchema, Subject: "test_x", Source: SourceDetector}} {
		if _, _, err := NewStore(pool).Propose(ctx, p); err == nil {
			t.Fatalf("%q was accepted", p.Subject)
		}
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("refused proposals notified %v", got)
	}
}

// Fleet: the notifier belongs to one pool; a Store on another pool (another
// monitored database) never reaches it.
func TestProposalNotifierIsPerPool(t *testing.T) {
	pool, ctx := livePool(t)
	n := listen(t, pool)
	other, err := pgxpool.New(ctx, livePoolDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	if _, created, err := NewStore(other).Propose(ctx, fixtureProposal("test_pp_*")); err !=
		nil || !created {
		t.Fatalf("propose on the other pool: created=%v err=%v", created, err)
	}
	if got := n.got(); len(got) != 0 {
		t.Fatalf("another pool's proposal reached this pool's notifier: %v", got)
	}
}

// Nil/zero: without a notifier a proposal is recorded as before; unsetting
// an older registration leaves a newer one in place.
func TestProposalNotifierUnset(t *testing.T) {
	pool, ctx := livePool(t)
	if _, created, err := NewStore(pool).Propose(ctx, fixtureProposal("test_none_*")); err !=
		nil || !created {
		t.Fatalf("propose without a notifier: created=%v err=%v", created, err)
	}
	old := &notifierLog{}
	unsetOld := SetProposalNotifier(pool, old.fn)
	n := listen(t, pool)
	unsetOld()
	unsetOld()
	if _, _, err := NewStore(pool).Propose(ctx, fixtureProposal("test_new_*")); err != nil {
		t.Fatal(err)
	}
	if len(old.got()) != 0 || len(n.got()) != 1 {
		t.Fatalf("old notified %v, new notified %v; want only the new one", old.got(),
			n.got())
	}
	SetProposalNotifier(pool, nil)()
	if _, _, err := NewStore(pool).Propose(ctx, fixtureProposal("test_nil_*")); err != nil {
		t.Fatalf("propose with a nil notifier: %v", err)
	}
	if len(n.got()) != 1 {
		t.Fatalf("a nil registration did not replace the notifier: %v", n.got())
	}
}

// Concurrent access: eight proposals of the same new fact at once send one
// card.
func TestProposalNotifierConcurrentProposalsSendOneCard(t *testing.T) {
	pool, ctx := livePool(t)
	n := listen(t, pool)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := NewStore(pool).Propose(ctx, fixtureProposal("test_race_*")); err !=
				nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent proposal: %v", err)
	}
	if got := n.got(); len(got) != 1 {
		t.Fatalf("eight concurrent proposals notified %v, want one card", got)
	}
	f, err := NewStore(pool).Get(ctx, n.got()[0])
	if err != nil || f.Proposals != 8 {
		t.Fatalf("fact after eight proposals: %+v %v", f, err)
	}
}
