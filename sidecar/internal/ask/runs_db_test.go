package ask

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// mcp-07b: asynchronous runs on a real sage schema with a fake model.
// A run starts (202), is polled, and ends in exactly one terminal state;
// its answer is stored as the synchronous Ask stores it.

func (f *fixture) runService(m *fakeLLM, edit func(*Deps)) *Service {
	f.t.Helper()
	d := f.deps(m)
	d.Instance = "host-a"
	if edit != nil {
		edit(&d)
	}
	return f.service(d)
}

func mustStart(t *testing.T, s *Service, c Caller, r Request) RunStart {
	t.Helper()
	st, err := s.StartRun(context.Background(), c, r)
	if err != nil {
		t.Fatalf("StartRun(%q): %v", r.Question, err)
	}
	if !uuidPattern.MatchString(st.RunID) || !uuidPattern.MatchString(st.ConversationID) {
		t.Fatalf("StartRun = %+v, want two uuids", st)
	}
	return st
}

// waitRun polls until until holds, recording every state seen.
func waitRun(t *testing.T, s *Service, c Caller, id string, until func(Run) bool) (Run,
	[]string) {
	t.Helper()
	var seen []string
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, err := s.PollRun(context.Background(), c, id)
		if err != nil {
			t.Fatalf("PollRun(%s): %v", id, err)
		}
		if len(seen) == 0 || seen[len(seen)-1] != r.State {
			seen = append(seen, r.State)
		}
		if until(r) {
			return r, seen
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s: states %v, condition never held", id, seen)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ended(r Run) bool { return r.Terminal() }

func groundedScript(t *testing.T, id int64, first time.Duration) []fakeReply {
	return []fakeReply{
		slowReply(first, calls(toolCall{"get_finding", `{"id":` + itoa(id) + `}`})),
		answer(func(body string) answerArgs {
			return answerArgs{Claims: []claimArg{{Text: "Finding " + itoa(id) +
				" reports 4200 sequential scans on public.orders.",
				EvidenceIDs: []string{aliasFor(t, body, "finding:"+itoa(id))}}}}
		})}
}

func TestRun_StartPollEndsDoneLikeTheSynchronousAnswer(t *testing.T) {
	f := newFixture(t)
	id := f.finding(findingSeed{Object: "public.orders", Title: "Missing index on orders",
		Detail: map[string]any{"seq_scans": 4200}})
	q := "Why is public.orders slow?"
	want := mustAsk(t, f.service(f.deps(newFakeLLM(t, groundedScript(t, id, 0)...))),
		viewer, q, "")
	s := f.runService(newFakeLLM(t, groundedScript(t, id, 400*time.Millisecond)...), nil)
	before := runCount("testdb", RunDone)
	st := mustStart(t, s, viewer, Request{Question: q})
	r, seen := waitRun(t, s, viewer, st.RunID, ended)
	if r.State != RunDone || r.MessageID == nil || r.Answer == nil || r.Error != "" ||
		r.FinishedAt == nil || r.ConversationID != st.ConversationID || r.Question != q {
		t.Fatalf("run = %+v", r)
	}
	if (seen[0] != RunQueued && seen[0] != RunRunning) || !containsState(seen, RunRunning) ||
		seen[len(seen)-1] != RunDone {
		t.Fatalf("states seen %v, want queued/running then done", seen)
	}
	if len(r.Progress) != 1 || r.Progress[0].Tool != "get_finding" ||
		r.Progress[0].Status == "" || r.Progress[0].At.IsZero() {
		t.Fatalf("progress = %+v", r.Progress)
	}
	stored, err := s.Thread(context.Background(), viewer, st.ConversationID)
	if err != nil || len(stored.Answers) != 1 || stored.Answers[0].ID != *r.MessageID {
		t.Fatalf("thread = %+v (%v)", stored, err)
	}
	a := stored.Answers[0]
	if a.Status != want.Status || a.Stop != want.Stop || a.Text != want.Text ||
		!reflect.DeepEqual(a.Statements, want.Statements) ||
		!reflect.DeepEqual(a.Citations, want.Citations) ||
		!reflect.DeepEqual(a.NotVerified, want.NotVerified) || a.Tokens != want.Tokens {
		t.Fatalf("stored run answer\n%+v\ndiffers from the synchronous one\n%+v", a, want)
	}
	if r.Answer.ID != a.ID || r.Answer.Text != a.Text {
		t.Fatalf("polled answer %+v is not the stored one", r.Answer)
	}
	if got := runCount("testdb", RunDone); got != before+1 {
		t.Fatalf("pg_sage_ask_runs_total{done} = %d, want %d", got, before+1)
	}
	if got := activeRuns("testdb"); got != 0 {
		t.Fatalf("pg_sage_ask_runs_active = %d after the run ended", got)
	}
}

func containsState(seen []string, state string) bool {
	for _, s := range seen {
		if s == state {
			return true
		}
	}
	return false
}

func TestRun_CancelWhileRunningKeepsThePartialAnswer(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, proposeThenSlow(5*time.Second), proposeThenSlow(5*time.Second))
	proposer := &fakeProposer{result: Proposal{QueueID: 9, Created: true,
		Verdict: "queue_approval", SQL: "CREATE INDEX CONCURRENTLY idx ON t (a)"}}
	s := f.runService(m, func(d *Deps) { d.Proposer = proposer })
	st := mustStart(t, s, operator, Request{Question: "Fix the slow orders query"})
	waitRun(t, s, operator, st.RunID, func(r Run) bool {
		return len(r.Progress) == 1 && m.calls() == 2
	})
	r, err := s.CancelRun(context.Background(), operator, st.RunID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if r.State != RunCancelled || r.MessageID == nil || r.Answer == nil {
		t.Fatalf("cancelled run = %+v", r)
	}
	if r.Answer.Status != StatusIncomplete || r.Answer.Stop != agentloop.StopCancelled ||
		len(r.Answer.Actions) != 1 || r.Answer.Actions[0].ID != "9" {
		t.Fatalf("partial answer = %+v", r.Answer)
	}
	if len(r.Progress) != 1 || r.Progress[0].Tool != "propose_action" ||
		r.Progress[0].ActionID != "9" {
		t.Fatalf("progress = %+v, want the proposal with its action id", r.Progress)
	}
	if _, err := s.CancelRun(context.Background(), operator, st.RunID); !errors.Is(err,
		ErrConflict) {
		t.Fatalf("a second cancel = %v, want ErrConflict", err)
	}
}

func TestRun_CancelBeforeAnyActionStoresNoMessage(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, calls(toolCall{"list_findings", `{}`}),
		slowReply(5*time.Second, answer(func(string) answerArgs { return answerArgs{} })))
	s := f.runService(m, nil)
	st := mustStart(t, s, viewer, Request{Question: "What is open?"})
	waitRun(t, s, viewer, st.RunID, func(r Run) bool { return m.calls() == 2 })
	r, err := s.CancelRun(context.Background(), viewer, st.RunID)
	if err != nil || r.State != RunCancelled || r.MessageID != nil || r.Answer != nil {
		t.Fatalf("CancelRun = %+v, %v", r, err)
	}
	if len(r.Progress) != 1 || r.Progress[0].Tool != "list_findings" ||
		r.Progress[0].ActionID != "" {
		t.Fatalf("progress = %+v", r.Progress)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_messages`); n != 0 {
		t.Fatalf("a cancelled read-only run stored %d answers", n)
	}
}

func TestRun_DoneRunCannotBeCancelled(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Nothing about invoices was observed."}}
	}))
	s := f.runService(m, nil)
	st := mustStart(t, s, viewer, Request{Question: "Invoices?"})
	r, _ := waitRun(t, s, viewer, st.RunID, ended)
	if r.State != RunDone {
		t.Fatalf("run = %+v", r)
	}
	if _, err := s.CancelRun(context.Background(), viewer, st.RunID); !errors.Is(err,
		ErrConflict) {
		t.Fatalf("cancel of a done run = %v, want ErrConflict", err)
	}
	again, err := s.PollRun(context.Background(), viewer, st.RunID)
	if err != nil || again.State != RunDone || again.MessageID == nil {
		t.Fatalf("after the refused cancel: %+v (%v)", again, err)
	}
}

func TestRun_NoToolCallsHasEmptyProgress(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Nothing was observed."}}
	}))
	s := f.runService(m, nil)
	st := mustStart(t, s, viewer, Request{Question: "Anything?"})
	r, _ := waitRun(t, s, viewer, st.RunID, ended)
	if r.Progress == nil || len(r.Progress) != 0 {
		t.Fatalf("progress = %#v, want an empty list", r.Progress)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"progress":[]`) {
		t.Fatalf("run JSON = %s", raw)
	}
	var stored string
	if err := f.pool.QueryRow(f.ctx, `SELECT progress::text FROM sage.ask_runs
		WHERE id = $1::uuid`, st.RunID).Scan(&stored); err != nil || stored != "[]" {
		t.Fatalf("stored progress = %q (%v)", stored, err)
	}
}

func TestRun_UnknownOrForeignRunIsNotFound(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Nothing was observed."}}
	}))
	s := f.runService(m, nil)
	st := mustStart(t, s, viewer, Request{Question: "Mine?"})
	waitRun(t, s, viewer, st.RunID, ended)
	unknown := "6f1c1a52-6d1f-4d55-9a43-6a0f3c1d2e3f"
	for _, c := range []struct {
		who Caller
		id  string
	}{{viewer, unknown}, {operator, st.RunID}} {
		if _, err := s.PollRun(context.Background(), c.who, c.id); !errors.Is(err,
			ErrNotFound) {
			t.Errorf("PollRun(%s, %s) = %v, want ErrNotFound", c.who.Actor, c.id, err)
		}
		if _, err := s.CancelRun(context.Background(), c.who, c.id); !errors.Is(err,
			ErrNotFound) {
			t.Errorf("CancelRun(%s, %s) = %v, want ErrNotFound", c.who.Actor, c.id, err)
		}
	}
	_, err := s.StartRun(context.Background(), operator, Request{Question: "Theirs?",
		ConversationID: st.ConversationID})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a run on another actor's conversation = %v, want ErrNotFound", err)
	}
}

func TestRun_TooLongQuestionInsertsNothing(t *testing.T) {
	f := newFixture(t)
	s := f.runService(nil, nil)
	_, err := s.StartRun(context.Background(), viewer,
		Request{Question: strings.Repeat("q", 8001)})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("8001-rune question = %v, want ErrInvalid", err)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_runs`) +
		f.count(`SELECT count(*) FROM sage.ask_conversations`); n != 0 {
		t.Fatalf("a refused question left %d rows", n)
	}
}

func TestRun_WallClockEndsIncomplete(t *testing.T) {
	// A model that answers one second after the question's wall clock.
	f := newFixture(t)
	m := newFakeLLM(t, slowReply(2*time.Second, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Too late."}}
	})))
	s := f.runService(m, func(d *Deps) {
		d.Budget = DefaultBudget()
		d.Budget.Wall = time.Second
	})
	st := mustStart(t, s, viewer, Request{Question: "Slow?"})
	r, _ := waitRun(t, s, viewer, st.RunID, ended)
	if r.State != RunDone || r.Answer == nil || r.Answer.Status != StatusIncomplete ||
		r.Answer.Stop != agentloop.StopWall {
		t.Fatalf("run = %+v answer %+v", r, r.Answer)
	}
}
