package ask

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// mcp-07: a run the caller cancels (browser closed, MCP client timeout)
// after it already took an action is stored as a partial answer with its
// actions and stop "cancelled" (status incomplete), so every queued
// proposal or opened investigation has the question that caused it. A
// run cancelled before any action stores nothing, as before.

const proposalMarker = "proposal 9 for finding"

// proposeThenSlow proposes finding 42 on a request that has not seen the
// proposal yet, and stalls (then gives up) on one that has.
func proposeThenSlow(stall time.Duration) fakeReply {
	propose := calls(toolCall{"propose_action", `{"finding_id":42}`})
	slow := slowReply(stall, answer(func(string) answerArgs { return answerArgs{} }))
	return func(w http.ResponseWriter, body string) {
		if strings.Contains(body, proposalMarker) {
			slow(w, body)
			return
		}
		propose(w, body)
	}
}

// cancelWhen cancels once cond holds (polled), failing the test after 10 s.
func cancelWhen(t *testing.T, cancel context.CancelFunc, cond func() bool) {
	t.Helper()
	go func() {
		defer cancel()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

func TestAsk_CancelAfterProposalStoresThePartialAnswer(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, proposeThenSlow(3*time.Second), proposeThenSlow(3*time.Second))
	proposer := &fakeProposer{result: Proposal{QueueID: 9, Created: true,
		Verdict: "queue_approval", SQL: "CREATE INDEX CONCURRENTLY idx ON t (a)"}}
	d := f.deps(m)
	d.Proposer = proposer
	s := f.service(d)
	before := answerCount("testdb", StatusIncomplete, agentloop.StopCancelled)
	ctx, cancel := context.WithCancel(context.Background())
	cancelWhen(t, cancel, func() bool { return proposer.count() == 1 && m.calls() == 2 })

	a, err := s.Ask(ctx, operator, Request{Question: "Fix the slow orders query"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if a.ID == 0 || a.ConversationID == "" || a.Status != StatusIncomplete ||
		a.Stop != agentloop.StopCancelled {
		t.Fatalf("partial answer = %+v", a)
	}
	if len(a.Actions) != 1 || a.Actions[0].Kind != ActionProposal || a.Actions[0].ID != "9" ||
		a.Actions[0].Status != ActionQueued {
		t.Fatalf("actions = %+v", a.Actions)
	}
	if !strings.Contains(a.Text, "cut short (cancelled)") ||
		!strings.Contains(a.Text, "Queued proposal 9") {
		t.Fatalf("text = %q", a.Text)
	}
	var status, stop, kind, id, question string
	if err := f.pool.QueryRow(f.ctx, `SELECT status, answer->>'stop',
		answer->'actions'->0->>'kind', answer->'actions'->0->>'id', question
		FROM sage.ask_messages WHERE id = $1`, a.ID).Scan(&status, &stop, &kind, &id,
		&question); err != nil {
		t.Fatalf("read the stored answer: %v", err)
	}
	if status != StatusIncomplete || stop != "cancelled" || kind != ActionProposal ||
		id != "9" || question != "Fix the slow orders query" {
		t.Fatalf("stored = %s %s %s %s %q", status, stop, kind, id, question)
	}
	if got := answerCount("testdb", StatusIncomplete, agentloop.StopCancelled); got !=
		before+1 {
		t.Fatalf("pg_sage_ask_answers_total{incomplete,cancelled} = %d, want %d", got,
			before+1)
	}
}

func TestAsk_CancelAfterAReadOnlyToolStoresNothing(t *testing.T) {
	// Evidence read is not an action: nothing exists that needs a record.
	f := newFixture(t)
	m := newFakeLLM(t, calls(toolCall{"list_findings", `{}`}),
		slowReply(3*time.Second, answer(func(string) answerArgs { return answerArgs{} })))
	proposer := &fakeProposer{}
	d := f.deps(m)
	d.Proposer = proposer
	s := f.service(d)
	ctx, cancel := context.WithCancel(context.Background())
	cancelWhen(t, cancel, func() bool { return m.calls() == 2 })

	a, err := s.Ask(ctx, operator, Request{Question: "What is open?"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if a.ID != 0 || len(a.Actions) != 0 || a.Status != "" {
		t.Fatalf("answer = %+v, want the zero answer", a)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_messages`); n != 0 {
		t.Fatalf("a cancelled read-only ask stored %d answers", n)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_conversations`); n != 0 {
		t.Fatalf("a cancelled read-only ask created %d conversations", n)
	}
}

// budgetVanishingProposer takes the budget table away while it proposes,
// so the next model call's reservation fails with ErrUnavailable.
type budgetVanishingProposer struct {
	fakeProposer
	pool *pgxpool.Pool
}

func (p *budgetVanishingProposer) ProposeFinding(ctx context.Context, id int64,
	actor string) (Proposal, error) {
	if _, err := p.pool.Exec(ctx, `ALTER TABLE sage.ask_budget_day
		RENAME TO ask_budget_day_away`); err != nil {
		return Proposal{}, fmt.Errorf("rename the budget table: %w", err)
	}
	return p.fakeProposer.ProposeFinding(ctx, id, actor)
}

func TestAsk_UnavailableBudgetAfterProposalStoresThePartialAnswer(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `ALTER TABLE IF EXISTS
			sage.ask_budget_day_away RENAME TO ask_budget_day`)
	})
	m := newFakeLLM(t, proposeThenSlow(0), proposeThenSlow(0))
	proposer := &budgetVanishingProposer{pool: f.pool,
		fakeProposer: fakeProposer{result: Proposal{QueueID: 9, Created: true}}}
	d := f.deps(m)
	d.Proposer = proposer
	a, err := f.service(d).Ask(context.Background(), operator,
		Request{Question: "Queue the index"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if m.calls() != 1 {
		t.Fatalf("model calls = %d, want 1 (the second reservation fails)", m.calls())
	}
	if a.ID == 0 || a.Status != StatusIncomplete || a.Stop != agentloop.StopCancelled ||
		len(a.Actions) != 1 || a.Actions[0].ID != "9" {
		t.Fatalf("partial answer = %+v", a)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_messages WHERE status = 'incomplete'
		AND answer->'actions'->0->>'id' = '9'`); n != 1 {
		t.Fatalf("stored partial answers = %d, want 1", n)
	}
}

func TestAsk_TwoCancelledRunsOnOneConversationBothSave(t *testing.T) {
	f := newFixture(t)
	first := newFakeLLM(t, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Nothing is slow."}}
	}))
	seed := mustAsk(t, f.service(f.deps(first)), operator, "Is anything slow?", "")
	reply := proposeThenSlow(3 * time.Second)
	m := newFakeLLM(t, reply, reply, reply, reply, reply, reply)
	proposer := &fakeProposer{result: Proposal{QueueID: 9, Created: true}}
	d := f.deps(m)
	d.Proposer = proposer
	s := f.service(d)
	ctx, cancel := context.WithCancel(context.Background())
	cancelWhen(t, cancel, func() bool { return proposer.count() == 2 && m.calls() == 4 })

	var wg sync.WaitGroup
	got := make([]Answer, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = s.Ask(ctx, operator, Request{Question: fmt.Sprintf(
				"Fix it %d", i), ConversationID: seed.ConversationID})
		}(i)
	}
	wg.Wait()
	for i := range got {
		if !errors.Is(errs[i], context.Canceled) || got[i].ID == 0 ||
			got[i].Status != StatusIncomplete || len(got[i].Actions) != 1 ||
			got[i].ConversationID != seed.ConversationID {
			t.Fatalf("run %d: %+v (%v)", i, got[i], errs[i])
		}
	}
	if got[0].ID == got[1].ID {
		t.Fatalf("both runs stored under id %d", got[0].ID)
	}
	assertThreadInOrder(t, s, seed)
}

// assertThreadInOrder checks seed's conversation holds seed and two more
// answers, oldest first.
func assertThreadInOrder(t *testing.T, s *Service, seed Answer) {
	t.Helper()
	thread, err := s.Thread(context.Background(), operator, seed.ConversationID)
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if len(thread.Answers) != 3 || thread.Conversation.Messages != 3 {
		t.Fatalf("thread has %d answers (%d messages), want 3", len(thread.Answers),
			thread.Conversation.Messages)
	}
	for i := 1; i < len(thread.Answers); i++ {
		if thread.Answers[i].ID <= thread.Answers[i-1].ID {
			t.Fatalf("answers out of order: %d after %d", thread.Answers[i].ID,
				thread.Answers[i-1].ID)
		}
	}
	if thread.Answers[0].ID != seed.ID || thread.Answers[0].Status != StatusNotObserved {
		t.Fatalf("the seed answer moved: %+v", thread.Answers[0])
	}
}

// answerCount is one series of pg_sage_ask_answers_total.
func answerCount(database, status, stop string) uint64 {
	for _, c := range AnswerCounts() {
		if c.Database == database && c.Status == status && c.Stop == stop {
			return c.Count
		}
	}
	return 0
}

func TestAsk_EveryAnswerIsCountedByStatusAndStop(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"No."}}
	}))
	before := answerCount("testdb", StatusNotObserved, agentloop.StopFinal)
	noModel := answerCount("testdb", StatusNoModel, agentloop.StopDisabled)
	mustAsk(t, f.service(f.deps(m)), viewer, "Why?", "")
	mustAsk(t, f.service(f.deps(nil)), viewer, "Why?", "")
	if got := answerCount("testdb", StatusNotObserved, agentloop.StopFinal); got !=
		before+1 {
		t.Fatalf("not_observed/final = %d, want %d", got, before+1)
	}
	if got := answerCount("testdb", StatusNoModel, agentloop.StopDisabled); got !=
		noModel+1 {
		t.Fatalf("llm_unavailable/llm_disabled = %d, want %d", got, noModel+1)
	}
	// An invalid question is an error, not an answer: nothing is counted.
	total := countAll()
	if _, err := f.service(f.deps(m)).Ask(context.Background(), viewer,
		Request{Question: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty question: %v", err)
	}
	if countAll() != total {
		t.Fatal("an invalid question was counted as an answer")
	}
}

func countAll() uint64 {
	var n uint64
	for _, c := range AnswerCounts() {
		n += c.Count
	}
	return n
}
