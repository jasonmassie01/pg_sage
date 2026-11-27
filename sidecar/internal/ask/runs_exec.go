package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// One run in the background: queued -> running -> one terminal state.
// The run reuses answer unchanged; a progress hook on the tool calls
// stores each call (one UPDATE per call) with the id of the action it
// took, and the answer is stored as Ask stores it.

// errCancelledElsewhere ends a run another sidecar marked cancelled.
var errCancelledElsewhere = errors.New("ask: the run was cancelled on another instance")

// runJob is what a started run answers.
type runJob struct {
	caller   Caller
	start    RunStart
	question string
	readOnly bool
	history  []Answer
}

// execute runs job to its end and stores the terminal state before it
// frees the run's slot, so a run no longer tracked here has ended.
func (s *Service) execute(ctx context.Context, ar *activeRun, job runJob) {
	state, msg, errText := RunFailed, (*int64)(nil), ""
	defer func() {
		if p := recover(); p != nil {
			state, msg, errText = RunFailed, nil, runError(fmt.Errorf("the run panicked: %v", p))
			s.d.Log("ERROR", "ask: run %s on %s panicked: %v", job.start.RunID, s.d.Database, p)
		}
		if err := s.store.finishRun(context.WithoutCancel(ctx), job.start.RunID, state, msg,
			errText); err != nil {
			s.d.Log("ERROR", "ask: run %s on %s ended %s but stays %s until a restart "+
				"recovers it: %v", job.start.RunID, s.d.Database, state, RunRunning, err)
		}
		s.runs.remove(job.start.RunID)
		runEnded(s.d.Database, state)
		ar.cancel()
		close(ar.done)
	}()
	state, msg, errText = s.runToEnd(ctx, ar, job)
}

// runToEnd answers the job and returns its terminal state, the stored
// answer's id and the error to store.
func (s *Service) runToEnd(ctx context.Context, ar *activeRun, job runJob) (string, *int64,
	string) {
	running, err := s.store.markRunning(ctx, job.start.RunID)
	switch {
	case err != nil:
		// A cancel or a shutdown that lands before the first write is
		// that, not a store failure.
		state := runOutcome(err, ar.cancelled.Load(), s.d.RunCtx.Err() != nil)
		return state, nil, s.stateError(state, err)
	case !running:
		return RunCancelled, nil, ""
	}
	pctx := withProgress(ctx, func(step RunStep) error {
		return s.recordStep(ctx, ar, job.start.RunID, step)
	})
	a, runErr := s.answer(pctx, job.caller, job.question, job.history)
	state := runOutcome(runErr, ar.cancelled.Load(), s.d.RunCtx.Err() != nil)
	errText := s.stateError(state, runErr)
	if runErr != nil && len(a.Actions) == 0 {
		return state, nil, errText
	}
	saved, err := s.persistAnswer(ctx, job.caller, job.start.ConversationID, job.question,
		job.readOnly, a)
	if err != nil {
		s.d.Log("ERROR", "ask: run %s of %s took %d actions and its answer could not be "+
			"stored: %v", job.start.RunID, job.caller.Actor, len(a.Actions), err)
		return RunFailed, nil, runError(err)
	}
	return state, &saved.ID, errText
}

// recordStep stores one tool call of the run. A store failure aborts the
// run; a row no longer running was cancelled on another instance.
func (s *Service) recordStep(ctx context.Context, ar *activeRun, id string,
	step RunStep) error {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	ok, err := s.store.appendProgress(pctx, id, step)
	if err != nil {
		return fmt.Errorf("%w: record the run's progress: %v", ErrUnavailable, err)
	}
	if !ok {
		ar.cancelled.Store(true)
		return errCancelledElsewhere
	}
	return nil
}

// stateError is the error stored with a terminal state.
func (s *Service) stateError(state string, runErr error) string {
	switch state {
	case RunFailed:
		return runError(runErr)
	case RunInterrupted:
		return "the sidecar stopped before the run ended"
	}
	return ""
}

// runOutcome is the terminal state of a run that returned err: done when
// it finished (a late cancel changes nothing), cancelled when its caller
// cancelled it, interrupted when the sidecar stopped, else failed.
func runOutcome(err error, cancelled, shutdown bool) string {
	switch {
	case err == nil:
		return RunDone
	case cancelled:
		return RunCancelled
	case shutdown:
		return RunInterrupted
	}
	return RunFailed
}

// runError is err as stored: at most MaxRunErrorRunes.
func runError(err error) string {
	if err == nil {
		return ""
	}
	return clip(err.Error(), MaxRunErrorRunes)
}

// persistAnswer stores a as the answer to question in conv (created when
// empty) and counts it, past the caller's own cancellation.
func (s *Service) persistAnswer(ctx context.Context, c Caller, conv, question string,
	readOnly bool, a Answer) (Answer, error) {
	a.ConversationID, a.Question, a.CreatedAt = conv, question, s.d.Now().UTC()
	a.ReadOnly = readOnly
	countAnswer(s.d.Database, a)
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	return s.store.save(saveCtx, c.Actor, a)
}

type progressKey struct{}

// withProgress makes the run's tool calls report to fn.
func withProgress(ctx context.Context, fn func(RunStep) error) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressFrom(ctx context.Context) func(RunStep) error {
	fn, _ := ctx.Value(progressKey{}).(func(RunStep) error)
	return fn
}

// tracked wraps each tool so that a run's progress hook sees every call:
// the tool, its status and the id of an action it took. Outside a run
// (no hook) the tools run unchanged.
func (ss *session) tracked(tools []agentloop.Tool) []agentloop.Tool {
	for i := range tools {
		name, run := tools[i].Name, tools[i].Run
		tools[i].Run = func(ctx context.Context, args json.RawMessage) (agentloop.Output,
			error) {
			report := progressFrom(ctx)
			if report == nil {
				return run(ctx, args)
			}
			before := len(ss.actionsTaken())
			out, err := run(ctx, args)
			step := RunStep{Tool: name, Status: stepStatus(out, err),
				ActionID: ss.actionSince(before), At: ss.s.d.Now().UTC()}
			if perr := report(step); perr != nil {
				return agentloop.Output{}, agentloop.Abort(perr)
			}
			return out, err
		}
	}
	return tools
}

func stepStatus(out agentloop.Output, err error) string {
	switch {
	case errors.Is(err, agentloop.ErrInvalidArgs):
		return "invalid_args"
	case err != nil:
		return "error"
	case out.Status != "":
		return clip(out.Status, 40)
	}
	return "ok"
}

// actionSince is the id of the last action taken after the first n.
func (ss *session) actionSince(n int) string {
	acts := ss.actionsTaken()
	if len(acts) <= n {
		return ""
	}
	return clip(acts[len(acts)-1].ID, 100)
}
