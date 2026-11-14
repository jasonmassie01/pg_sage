package ask

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// mcp-07b: run caps, restart recovery, a store outage mid-run, shutdown,
// a cancel from another instance, and pollers racing a cancel.

func slowAnswers(n int, d time.Duration) []fakeReply {
	out := make([]fakeReply, n)
	for i := range out {
		out[i] = slowReply(d, answer(func(string) answerArgs {
			return answerArgs{NotObserved: []string{"Nothing was observed."}}
		}))
	}
	return out
}

func TestRun_FifthConcurrentRunIsBusy(t *testing.T) {
	f := newFixture(t)
	s := f.runService(newFakeLLM(t, slowAnswers(5, 5*time.Second)...), func(d *Deps) {
		d.Config.MaxConcurrentRuns = 4
	})
	var started []RunStart
	for range 4 {
		started = append(started, mustStart(t, s, viewer, Request{Question: "Slow?"}))
	}
	if got := activeRuns("testdb"); got != 4 {
		t.Fatalf("pg_sage_ask_runs_active = %d, want 4", got)
	}
	_, err := s.StartRun(context.Background(), viewer, Request{Question: "Fifth?"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("the fifth run = %v, want ErrBusy", err)
	}
	if n := f.count(`SELECT count(*) FROM sage.ask_runs`); n != 4 {
		t.Fatalf("%d runs stored, want 4 (the refused one inserts nothing)", n)
	}
	if _, err := s.CancelRun(context.Background(), viewer, started[0].RunID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	mustStart(t, s, viewer, Request{Question: "Now?"})
	for _, st := range started[1:] {
		if _, err := s.CancelRun(context.Background(), viewer, st.RunID); err != nil {
			t.Fatalf("cancel %s: %v", st.RunID, err)
		}
	}
}

func TestRun_SecondRunOnOneConversationConflicts(t *testing.T) {
	f := newFixture(t)
	s := f.runService(newFakeLLM(t, slowAnswers(2, 600*time.Millisecond)...), nil)
	first := mustStart(t, s, viewer, Request{Question: "First?"})
	_, err := s.StartRun(context.Background(), viewer, Request{Question: "Second?",
		ConversationID: first.ConversationID})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a second run on the conversation = %v, want ErrConflict", err)
	}
	waitRun(t, s, viewer, first.RunID, ended)
	next := mustStart(t, s, viewer, Request{Question: "Second?",
		ConversationID: first.ConversationID})
	if next.ConversationID != first.ConversationID {
		t.Fatalf("the follow-up moved to conversation %s", next.ConversationID)
	}
	r, _ := waitRun(t, s, viewer, next.RunID, ended)
	if r.State != RunDone {
		t.Fatalf("follow-up run = %+v", r)
	}
}

// seedRun inserts a run row of instance whose updated_at is age old.
func (f *fixture) seedRun(instance, state string, age time.Duration, progress string) string {
	f.t.Helper()
	var conv, id string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.ask_conversations (actor, title)
		VALUES ('user:7', 'seeded') RETURNING id::text`).Scan(&conv); err != nil {
		f.t.Fatalf("seed conversation: %v", err)
	}
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.ask_runs (id, conversation_id,
		actor, question, read_only, state, progress, instance, created_at, updated_at)
		VALUES (gen_random_uuid(), $1::uuid, 'user:7', 'q', false, $2, $3::jsonb, $4,
		now() - $5::interval, now() - $5::interval) RETURNING id::text`, conv, state,
		progress, instance, age.String()).Scan(&id); err != nil {
		f.t.Fatalf("seed run: %v", err)
	}
	return id
}

func (f *fixture) runState(id string) (state string, progress string, finished bool) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, `SELECT state, progress::text, finished_at IS NOT NULL
		FROM sage.ask_runs WHERE id = $1::uuid`, id).Scan(&state, &progress,
		&finished); err != nil {
		f.t.Fatalf("read run %s: %v", id, err)
	}
	return state, progress, finished
}

func TestRecoverRuns_StaleRunsOfThisInstanceAreInterrupted(t *testing.T) {
	f := newFixture(t)
	acted := `[{"tool":"propose_action","status":"queued","action_id":"9",` +
		`"at":"2026-11-13T10:00:00Z"}]`
	stale := f.seedRun("host-a", RunRunning, 10*time.Minute, acted)
	queued := f.seedRun("host-a", RunQueued, 10*time.Minute, "[]")
	other := f.seedRun("host-b", RunRunning, 10*time.Minute, "[]")
	fresh := f.seedRun("host-a", RunRunning, 5*time.Second, "[]")
	done := f.seedRun("host-a", RunDone, time.Hour, "[]")
	s := f.runService(nil, nil)
	n, err := s.RecoverRuns(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("RecoverRuns = %d, %v; want 2", n, err)
	}
	state, progress, finished := f.runState(stale)
	if state != RunInterrupted || !finished || !strings.Contains(progress, `"action_id": "9"`) {
		t.Fatalf("stale run = %s %v %s", state, finished, progress)
	}
	if state, _, _ := f.runState(queued); state != RunInterrupted {
		t.Fatalf("stale queued run = %s", state)
	}
	for id, want := range map[string]string{other: RunRunning, fresh: RunRunning,
		done: RunDone} {
		if state, _, _ := f.runState(id); state != want {
			t.Errorf("run %s = %s, want %s untouched", id, state, want)
		}
	}
	// A fake clock two hours on: the fresh row is stale now too.
	later := f.runService(nil, func(d *Deps) {
		d.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	})
	if n, err := later.RecoverRuns(context.Background()); err != nil || n != 1 {
		t.Fatalf("RecoverRuns later = %d, %v; want 1", n, err)
	}
	if state, _, _ := f.runState(fresh); state != RunInterrupted {
		t.Fatalf("fresh run after the clock moved = %s", state)
	}
	if state, _, _ := f.runState(other); state != RunRunning {
		t.Fatalf("another instance's run = %s, want running", state)
	}
}

// failProgress makes every progress update fail with a long error.
func (f *fixture) failProgress() {
	f.t.Helper()
	f.exec(`CREATE OR REPLACE FUNCTION sage.test_fail_progress() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		RAISE EXCEPTION 'progress store down: %', repeat('x', 700); END $$`)
	f.exec(`CREATE TRIGGER test_fail_progress BEFORE UPDATE ON sage.ask_runs FOR EACH ROW
		WHEN (NEW.progress IS DISTINCT FROM OLD.progress)
		EXECUTE FUNCTION sage.test_fail_progress()`)
	f.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = f.pool.Exec(ctx, `DROP TRIGGER IF EXISTS test_fail_progress ON sage.ask_runs`)
		_, _ = f.pool.Exec(ctx, `DROP FUNCTION IF EXISTS sage.test_fail_progress()`)
	})
}

func TestRun_StoreOutageDuringProgressEndsFailed(t *testing.T) {
	f := newFixture(t)
	f.failProgress()
	m := newFakeLLM(t, calls(toolCall{"list_findings", `{}`}), slowAnswers(1, 0)[0])
	s := f.runService(m, nil)
	st := mustStart(t, s, viewer, Request{Question: "What is open?"})
	r, _ := waitRun(t, s, viewer, st.RunID, ended)
	if r.State != RunFailed || r.MessageID != nil || r.Answer != nil {
		t.Fatalf("run = %+v", r)
	}
	if n := utf8.RuneCountInString(r.Error); n == 0 || n > MaxRunErrorRunes ||
		!strings.Contains(r.Error, "progress store down") {
		t.Fatalf("stored error (%d runes) = %q", n, r.Error)
	}
	if len(r.Progress) != 0 {
		t.Fatalf("progress = %+v, want none recorded", r.Progress)
	}
	if m.calls() != 1 {
		t.Fatalf("the model was called %d times after the store failed", m.calls())
	}
}

func TestRun_ShutdownInterruptsTheRun(t *testing.T) {
	f := newFixture(t)
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	m := newFakeLLM(t, slowAnswers(1, 5*time.Second)...)
	s := f.runService(m, func(d *Deps) { d.RunCtx = runCtx })
	st := mustStart(t, s, viewer, Request{Question: "Slow?"})
	waitRun(t, s, viewer, st.RunID, func(Run) bool { return m.calls() == 1 })
	stop()
	r, _ := waitRun(t, s, viewer, st.RunID, ended)
	if r.State != RunInterrupted || r.MessageID != nil {
		t.Fatalf("run after shutdown = %+v", r)
	}
}

func TestRun_CancelFromAnotherInstanceStopsTheRun(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, slowReply(700*time.Millisecond, calls(toolCall{"list_findings",
		`{}`})), slowAnswers(1, 0)[0])
	owner := f.runService(m, nil)
	peer := f.runService(nil, func(d *Deps) { d.Instance = "host-b" })
	st := mustStart(t, owner, viewer, Request{Question: "What is open?"})
	// The cancel lands while the owner waits on the model's first reply.
	waitRun(t, owner, viewer, st.RunID, func(Run) bool { return m.calls() == 1 })
	r, err := peer.CancelRun(context.Background(), viewer, st.RunID)
	if err != nil || r.State != RunCancelled {
		t.Fatalf("peer CancelRun = %+v, %v", r, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for owner.runs.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if owner.runs.count() != 0 {
		t.Fatal("the owner kept running a run cancelled on another instance")
	}
	if m.calls() != 1 {
		t.Fatalf("the model was called %d times after the cancel", m.calls())
	}
	if state, _, _ := f.runState(st.RunID); state != RunCancelled {
		t.Fatalf("state = %s, want cancelled", state)
	}
}

func TestRun_PollersAndCancelEndInOneTerminalState(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, calls(toolCall{"list_findings", `{}`}), slowAnswers(1, 300*
		time.Millisecond)[0])
	s := f.runService(m, nil)
	st := mustStart(t, s, viewer, Request{Question: "Race?"})
	var wg sync.WaitGroup
	terminals := make([][]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				r, err := s.PollRun(context.Background(), viewer, st.RunID)
				if err != nil {
					t.Errorf("poller %d: %v", i, err)
					return
				}
				if r.Terminal() {
					terminals[i] = append(terminals[i], r.State)
					if len(terminals[i]) == 5 {
						return
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	time.Sleep(250 * time.Millisecond)
	_, cancelErr := s.CancelRun(context.Background(), viewer, st.RunID)
	wg.Wait()
	final, _, _ := f.runState(st.RunID)
	if final != RunCancelled && final != RunDone {
		t.Fatalf("final state = %s", final)
	}
	if final == RunDone && !errors.Is(cancelErr, ErrConflict) {
		t.Fatalf("the run ended done but the cancel said %v", cancelErr)
	}
	for i, seen := range terminals {
		if len(seen) == 0 {
			t.Fatalf("poller %d never saw a terminal state", i)
		}
		for _, s := range seen {
			if s != final {
				t.Fatalf("poller %d saw terminal states %v, final %s", i, seen, final)
			}
		}
	}
}
