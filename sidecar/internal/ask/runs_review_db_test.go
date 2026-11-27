package ask

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// mcp-07b review: a run stopped before it is marked running, and runs a
// fast restart left behind (still inside the stale window at start).

// queueOnly stores a queued run of question the way StartRun does, without
// starting it, and returns the job a started run would answer.
func queueOnly(t *testing.T, s *Service, question string) runJob {
	t.Helper()
	id, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	c := viewer.For(Request{Question: question})
	st, history, err := s.queueRun(context.Background(), c, id, question,
		Request{Question: question})
	if err != nil {
		t.Fatalf("queueRun: %v", err)
	}
	return runJob{caller: c, start: st, question: question, history: history}
}

func TestRun_CancelBeforeItIsMarkedRunningIsCancelled(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, slowAnswers(1, 0)...)
	s := f.runService(m, nil)
	job := queueOnly(t, s, "Cancelled at once?")
	ctx, cancel := context.WithCancel(context.Background())
	ar := &activeRun{cancel: cancel, done: make(chan struct{})}
	// The cancel lands between the start and the run's first write.
	ar.cancelled.Store(true)
	cancel()
	s.execute(ctx, ar, job)
	r, err := s.PollRun(context.Background(), viewer, job.start.RunID)
	if err != nil {
		t.Fatalf("PollRun: %v", err)
	}
	if r.State != RunCancelled || r.Error != "" || r.MessageID != nil {
		t.Fatalf("run cancelled before it started = %+v, want cancelled with no error", r)
	}
	if m.calls() != 0 {
		t.Fatalf("the model was called %d times for a cancelled run", m.calls())
	}
}

func TestRun_ShutdownBeforeItIsMarkedRunningIsInterrupted(t *testing.T) {
	f := newFixture(t)
	runCtx, stop := context.WithCancel(context.Background())
	s := f.runService(newFakeLLM(t, slowAnswers(1, 0)...), func(d *Deps) {
		d.RunCtx = runCtx
	})
	job := queueOnly(t, s, "Shut down at once?")
	ctx, cancel := context.WithCancel(runCtx)
	ar := &activeRun{cancel: cancel, done: make(chan struct{})}
	stop()
	s.execute(ctx, ar, job)
	r, err := s.PollRun(context.Background(), viewer, job.start.RunID)
	if err != nil {
		t.Fatalf("PollRun: %v", err)
	}
	if r.State != RunInterrupted {
		t.Fatalf("run stopped by a shutdown before it started = %+v, want interrupted", r)
	}
}

func TestWatchAbandonedRuns_RecoversRunsThatGoStaleAfterStart(t *testing.T) {
	f := newFixture(t)
	// Left by a sidecar that restarted seconds ago: not yet stale at start.
	fresh := f.seedRun("host-a", RunRunning, 5*time.Second, "[]")
	other := f.seedRun("host-b", RunRunning, time.Hour, "[]")
	var skew atomic.Int64
	s := f.runService(nil, func(d *Deps) {
		d.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.WatchAbandonedRuns(ctx, 20*time.Millisecond)
	}()
	time.Sleep(100 * time.Millisecond)
	if state, _, _ := f.runState(fresh); state != RunRunning {
		t.Fatalf("a run inside the stale window = %s, want running", state)
	}
	skew.Store(int64(2 * time.Hour)) // the stale window passes
	deadline := time.Now().Add(10 * time.Second)
	for {
		if state, _, _ := f.runState(fresh); state == RunInterrupted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run left by the restart was never marked interrupted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state, _, _ := f.runState(other); state != RunRunning {
		t.Fatalf("another instance's run = %s, want running", state)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchAbandonedRuns did not return when its context ended")
	}
}

func TestRecoverRuns_LeavesThisProcessLiveRunsAloneUnderClockSkew(t *testing.T) {
	f := newFixture(t)
	var skew atomic.Int64
	m := newFakeLLM(t, slowAnswers(1, 1500*time.Millisecond)...)
	s := f.runService(m, func(d *Deps) {
		d.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	})
	st := mustStart(t, s, viewer, Request{Question: "Live?"})
	waitRun(t, s, viewer, st.RunID, func(Run) bool { return m.calls() == 1 })
	// The sidecar's clock jumps far ahead of the database's: by the clock
	// alone the live run looks abandoned.
	skew.Store(int64(2 * time.Hour))
	n, err := s.RecoverRuns(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("RecoverRuns during a live run = %d, %v; want 0", n, err)
	}
	r, _ := waitRun(t, s, viewer, st.RunID, ended)
	if r.State != RunDone || r.MessageID == nil {
		t.Fatalf("live run after a sweep = %+v, want done with its answer", r)
	}
}

func TestRun_CancelledOnAnotherInstanceKeepsThePartialAnswer(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, calls(toolCall{"propose_action", `{"finding_id":42}`}),
		slowReply(800*time.Millisecond, calls(toolCall{"list_findings", `{}`})),
		slowAnswers(1, 0)[0])
	proposer := &fakeProposer{result: Proposal{QueueID: 9, Created: true,
		Verdict: "queue_approval", SQL: "CREATE INDEX CONCURRENTLY idx ON t (a)"}}
	owner := f.runService(m, func(d *Deps) { d.Proposer = proposer })
	peer := f.runService(nil, func(d *Deps) { d.Instance = "host-b" })
	st := mustStart(t, owner, operator, Request{Question: "Fix the slow orders query"})
	// The cancel lands on the peer after the proposal, while the owner
	// waits on the model; the owner sees it at its next tool call.
	waitRun(t, owner, operator, st.RunID, func(r Run) bool {
		return len(r.Progress) == 1 && m.calls() == 2
	})
	if _, err := peer.CancelRun(context.Background(), operator, st.RunID); err != nil {
		t.Fatalf("peer CancelRun: %v", err)
	}
	r, _ := waitRun(t, owner, operator, st.RunID, func(r Run) bool {
		return r.MessageID != nil || owner.runs.count() == 0
	})
	if r.State != RunCancelled || r.MessageID == nil || r.Answer == nil {
		t.Fatalf("run cancelled on the peer = %+v, want cancelled with the partial answer", r)
	}
	if len(r.Answer.Actions) != 1 || r.Answer.Actions[0].ID != "9" ||
		r.Answer.Stop != agentloop.StopCancelled {
		t.Fatalf("partial answer = %+v", r.Answer)
	}
	if m.calls() != 2 {
		t.Fatalf("the model was called %d times after the cancel", m.calls())
	}
}
