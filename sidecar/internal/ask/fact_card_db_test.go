package ask

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/facts"
)

// facts-09: a fact Ask Sage proposes reaches the pool's proposal notifier
// (the runtime sends its fact card) once; asking again about the same
// fact notifies nothing.

type askCards struct {
	mu  sync.Mutex
	ids []int64
}

func (c *askCards) note(_ context.Context, f facts.Fact) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, f.ID)
}

func (c *askCards) got() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.ids...)
}

func askForFact(t *testing.T, f *fixture, findingID int64) Answer {
	t.Helper()
	evID := "finding:" + itoa(findingID)
	m := newFakeLLM(t, calls(toolCall{"get_finding", `{"id":` + itoa(findingID) + `}`}),
		calls(toolCall{"propose_fact", factArgs("public.idx_card", evID)}),
		answer(func(string) answerArgs {
			return answerArgs{NotObserved: []string{"A person confirms the fact."}}
		}))
	return mustAsk(t, f.service(f.deps(m)), operator, "Is idx_card owned by the app?", "")
}

func TestFact_AskProposalNotifiesOnce(t *testing.T) {
	f := newFixture(t)
	cards := &askCards{}
	t.Cleanup(facts.SetProposalNotifier(f.pool, cards.note))
	id := f.finding(findingSeed{Object: "public.idx_card", Title: "Index dropped and back"})
	first := askForFact(t, f, id)
	if len(first.Actions) != 1 || first.Actions[0].Status != ActionProposedFact {
		t.Fatalf("actions = %+v", first.Actions)
	}
	got := cards.got()
	if len(got) != 1 || itoa(got[0]) != first.Actions[0].ID {
		t.Fatalf("Ask's proposal notified %v, want [%s]", got, first.Actions[0].ID)
	}
	second := askForFact(t, f, id)
	if len(second.Actions) != 1 || second.Actions[0].Status != ActionPending {
		t.Fatalf("second ask actions = %+v", second.Actions)
	}
	if got := cards.got(); len(got) != 1 {
		t.Fatalf("asking again about the same fact notified %v", got)
	}
}
