package rollout

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const (
	// defaultVerdictPoll is how often a measurement re-reads a target's
	// verdict while the target still monitors the change.
	defaultVerdictPoll = 15 * time.Second
	// verdictGrace is how long past the target's monitor deadline the
	// measurement still waits for the verdict to be written.
	verdictGrace = 2 * time.Minute
)

// inProgressOutcomes are the action outcomes the target's own monitor has
// not settled: a reversible action stays "monitoring" for its window, an
// interrupted monitor resumes when its sidecar restarts, and a rollback in
// progress ("rolling_back") ends rolled_back or rollback_failed.
var inProgressOutcomes = map[string]bool{"monitoring": true, "pending": true,
	"interrupted": true, "rolling_back": true}

// finalVerifications are the verification verdicts that end a watch.
var finalVerifications = map[string]bool{"success": true, "revert": true, "failed": true,
	"unverifiable": true}

// Measure waits for the target's own verdict of the change: it reads the
// action's outcome every PollEvery until the verdict is final, and judges
// it then. The wait ends at the target's monitor deadline plus
// verdictGrace; a verdict still open then is reported pending, unjudged,
// and the change stays with the target's monitor.
func (b *canaryBackend) Measure(ctx context.Context, instance Instance,
	change AppliedChange) (Outcome, error) {
	actionID, err := strconv.ParseInt(change.Handle, 10, 64)
	if err != nil || actionID <= 0 {
		return Outcome{}, fmt.Errorf("change handle %q on %s is not an action id",
			change.Handle, instance.ID)
	}
	target, err := b.svc.resolve(instance.ID)
	if err != nil {
		return Outcome{}, err
	}
	deadline, err := target.MonitorDeadline(ctx, actionID)
	if err != nil {
		return Outcome{}, fmt.Errorf("monitor deadline of action %d on %s: %w", actionID,
			instance.ID, err)
	}
	deadline = deadline.Add(verdictGrace)
	for {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		outcome, verification, err := target.ActionResult(ctx, actionID)
		if err != nil {
			return Outcome{}, fmt.Errorf("read the verdict of action %d on %s: %w", actionID,
				instance.ID, err)
		}
		if verdictFinal(outcome, verification) {
			b.markPauseDue()
			return b.withEffect(ctx, instance.ID, target, actionID,
				judgeVerdict(change.Handle, outcome, verification)), nil
		}
		left := deadline.Sub(b.svc.opts.Now())
		if left <= 0 {
			return pendingVerdict(change.Handle, outcome, verification, deadline), nil
		}
		if err := b.svc.wait(ctx, min(b.svc.opts.PollEvery, left)); err != nil {
			return Outcome{}, err
		}
	}
}

// verdictFinal reports whether the target's monitor has settled the action.
func verdictFinal(outcome, verification string) bool {
	return finalVerifications[verification] || !inProgressOutcomes[outcome]
}

// judgeVerdict judges a settled action: anything but a kept success with
// no failed, reverted or unverifiable verification fails the instance.
func judgeVerdict(handle, outcome, verification string) Outcome {
	o := Outcome{EvidenceID: "action_log:" + handle,
		Detail: fmt.Sprintf("outcome %s, verification %q", outcome, verification)}
	switch verification {
	case "failed", "revert", "unverifiable":
		o.Failed = true
	}
	if outcome != "success" {
		o.Failed = true
	}
	if o.Failed {
		o.RegressionPct = 100
	}
	return o
}

func pendingVerdict(handle, outcome, verification string, deadline time.Time) Outcome {
	return Outcome{EvidenceID: "action_log:" + handle, Pending: true,
		Detail: fmt.Sprintf("outcome %s, verification %q: still monitoring at the deadline "+
			"%s; the database's own monitor keeps the change and gives its verdict",
			outcome, verification, deadline.UTC().Format(time.RFC3339))}
}
