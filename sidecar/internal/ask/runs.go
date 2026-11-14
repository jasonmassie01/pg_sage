package ask

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Asynchronous runs (mcp-07b): a question answered in the background on
// the service's own context, so it outlives the request that started it.
// A run is stored in sage.ask_runs with its state and the tools it called
// so far; its answer is stored exactly as Ask stores one. One active run
// per conversation, at most ask.max_concurrent_runs per database on this
// sidecar.

// Run states.
const (
	RunQueued      = "queued"
	RunRunning     = "running"
	RunDone        = "done"
	RunCancelled   = "cancelled"
	RunInterrupted = "interrupted"
	RunFailed      = "failed"
)

const (
	// MaxRunErrorRunes bounds the stored error of a failed run.
	MaxRunErrorRunes = 500
	// maxRunSteps bounds a run's stored progress.
	maxRunSteps = 64
	// staleRunSlack is how long past its wall clock a run may go without
	// an update before its row counts as abandoned.
	staleRunSlack = 30 * time.Second
	// cancelWait bounds how long a cancel waits for the run to end.
	cancelWait = persistTimeout + 2*time.Second
	// recoverTimeout bounds one sweep for abandoned runs.
	recoverTimeout = 30 * time.Second
)

// RunStart is a started run.
type RunStart struct {
	RunID          string `json:"run_id"`
	ConversationID string `json:"conversation_id"`
}

// RunStep is one tool call of a run: the tool, its status and the id of
// the action it took. Never the tool's output.
type RunStep struct {
	Tool     string    `json:"tool"`
	Status   string    `json:"status"`
	ActionID string    `json:"action_id,omitempty"`
	At       time.Time `json:"at"`
}

// Run is a run as polled: its state, progress and, once stored, answer.
type Run struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	State          string     `json:"state"`
	Question       string     `json:"question"`
	ReadOnly       bool       `json:"read_only"`
	Progress       []RunStep  `json:"progress"`
	MessageID      *int64     `json:"message_id,omitempty"`
	Answer         *Answer    `json:"answer,omitempty"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

// Terminal reports whether the run has ended.
func (r Run) Terminal() bool {
	switch r.State {
	case RunDone, RunCancelled, RunInterrupted, RunFailed:
		return true
	}
	return false
}

// activeRun is a run executing on this sidecar.
type activeRun struct {
	cancel    context.CancelFunc
	cancelled atomic.Bool
	done      chan struct{}
}

// runTracker holds this sidecar's active runs of one database, at most
// limit at once.
type runTracker struct {
	mu     sync.Mutex
	limit  int
	active map[string]*activeRun
}

func newRunTracker(limit int) *runTracker {
	return &runTracker{limit: limit, active: map[string]*activeRun{}}
}

func (t *runTracker) add(id string, ar *activeRun) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.active) >= t.limit {
		return fmt.Errorf("%w: %d runs are in progress on this database "+
			"(ask.max_concurrent_runs); try again shortly", ErrBusy, len(t.active))
	}
	t.active[id] = ar
	return nil
}

func (t *runTracker) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.active, id)
}

func (t *runTracker) get(id string) *activeRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active[id]
}

// ids are the runs active here.
func (t *runTracker) ids() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.active))
	for id := range t.active {
		out = append(out, id)
	}
	return out
}

func (t *runTracker) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.active)
}

// newRunID is a random (version 4) UUID.
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("%w: generate a run id: %v", ErrUnavailable, err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// staleBefore is the update time before which a queued or running row
// is abandoned: no live run goes that long without one.
func (s *Service) staleBefore() time.Time {
	return s.d.Now().Add(-(s.d.Budget.Wall + deadlineMargin + staleRunSlack))
}

// StartRun stores a queued run of the caller's question (creating the
// conversation when none is given) and answers it in the background on
// the service's run context, bounded by the question's wall clock. Busy
// (too many runs on this database) is ErrBusy; a conversation with an
// active run is ErrConflict.
func (s *Service) StartRun(ctx context.Context, c Caller, r Request) (RunStart, error) {
	if !s.d.Config.Enabled {
		return RunStart{}, ErrDisabled
	}
	c = c.For(r)
	question, err := checkRequest(c, r)
	if err != nil {
		return RunStart{}, err
	}
	if r.ConversationID != "" && !uuidPattern.MatchString(r.ConversationID) {
		return RunStart{}, fmt.Errorf("%w: conversation %q", ErrNotFound, r.ConversationID)
	}
	id, err := newRunID()
	if err != nil {
		return RunStart{}, err
	}
	runCtx, cancel := context.WithTimeout(s.d.RunCtx, s.d.Budget.Wall+deadlineMargin)
	ar := &activeRun{cancel: cancel, done: make(chan struct{})}
	if err := s.runs.add(id, ar); err != nil {
		cancel()
		return RunStart{}, err
	}
	started, history, err := s.queueRun(ctx, c, id, question, r)
	if err != nil {
		s.runs.remove(id)
		cancel()
		return RunStart{}, err
	}
	runStarted(s.d.Database)
	go s.execute(runCtx, ar, runJob{caller: c, start: started, question: question,
		readOnly: r.ReadOnly, history: history})
	return started, nil
}

// queueRun inserts the run and reads the conversation's history.
func (s *Service) queueRun(ctx context.Context, c Caller, id, question string,
	r Request) (RunStart, []Answer, error) {
	conv, err := s.store.insertRun(ctx, runInsert{id: id, actor: c.Actor,
		conversation: r.ConversationID, question: question, readOnly: r.ReadOnly,
		instance: s.d.Instance, staleBefore: s.staleBefore()})
	if err != nil {
		return RunStart{}, nil, err
	}
	var history []Answer
	if r.ConversationID != "" {
		if history, err = s.store.history(ctx, conv, historyTurns); err != nil {
			if ferr := s.store.finishRun(context.WithoutCancel(ctx), id, RunFailed, nil,
				runError(err)); ferr != nil {
				s.d.Log("WARN", "ask: run %s could not read its history and could not be "+
					"marked failed: %v", id, ferr)
			}
			return RunStart{}, nil, err
		}
	}
	return RunStart{RunID: id, ConversationID: conv}, history, nil
}

// PollRun is one of the caller's runs; another actor's is ErrNotFound.
func (s *Service) PollRun(ctx context.Context, c Caller, id string) (Run, error) {
	if !uuidPattern.MatchString(id) {
		return Run{}, fmt.Errorf("%w: run %q", ErrNotFound, id)
	}
	if err := checkActor(c.Actor); err != nil {
		return Run{}, err
	}
	return s.store.run(ctx, c.Actor, id)
}

// CancelRun cancels one of the caller's runs and returns it as it ended.
// A run that already ended is ErrConflict. A run of another sidecar is
// marked cancelled; that sidecar stops it at its next tool call.
func (s *Service) CancelRun(ctx context.Context, c Caller, id string) (Run, error) {
	r, err := s.PollRun(ctx, c, id)
	if err != nil {
		return Run{}, err
	}
	if r.Terminal() {
		return Run{}, fmt.Errorf("%w: the run already ended (%s)", ErrConflict, r.State)
	}
	if ar := s.runs.get(id); ar != nil {
		ar.cancelled.Store(true)
		ar.cancel()
		select {
		case <-ar.done:
		case <-time.After(cancelWait):
		case <-ctx.Done():
		}
	} else if err := s.store.cancelRun(ctx, id); err != nil {
		return Run{}, err
	}
	if r, err = s.PollRun(ctx, c, id); err == nil && r.Terminal() && r.State != RunCancelled {
		return Run{}, fmt.Errorf("%w: the run ended (%s) before the cancel took effect",
			ErrConflict, r.State)
	}
	return r, err
}

// RecoverRuns marks this instance's abandoned queued or running runs
// interrupted (their progress stays as the record) and returns how many.
// Runs of other instances are left alone, and so are the runs active in
// this process whatever the clocks say.
func (s *Service) RecoverRuns(ctx context.Context) (int64, error) {
	n, err := s.store.interruptStale(ctx, s.d.Instance, s.staleBefore(), s.runs.ids())
	if err == nil && n > 0 {
		s.d.Log("WARN", "ask: %d runs of %s on %s were abandoned (restart) and are "+
			"marked interrupted", n, s.d.Instance, s.d.Database)
	}
	return n, err
}

// WatchAbandonedRuns recovers this instance's abandoned runs now and then
// every interval until ctx ends. A sidecar restarted inside the stale
// window finds its old rows still fresh at start; they go stale later,
// and without a later sweep they would stay running for good.
func (s *Service) WatchAbandonedRuns(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		sweep, cancel := context.WithTimeout(ctx, recoverTimeout)
		if _, err := s.RecoverRuns(sweep); err != nil && ctx.Err() == nil {
			s.d.Log("WARN", "ask: %s could not recover its abandoned runs on %s: %v",
				s.d.Instance, s.d.Database, err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
