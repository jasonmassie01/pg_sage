package approvalcard

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// One change per object: a queued change whose object (a GUC, or a table
// and its indexes) has another change still being verified shows that
// wait, and that approving it overrides the verification (the gate
// records the override on the decision). The wait is read live, as verdicts
// land; it is not part of the content a decision is bound to.

// WaitSource lists the changes in flight on the objects a change names.
// *executor.VerificationWaits is one.
type WaitSource interface {
	PendingFor(ctx context.Context, sql string, targets []string) (
		[]policy.PendingVerification, error)
}

// VerificationWait is the card's wait. Line is the one line chat shows.
type VerificationWait struct {
	ActionIDs   []int64    `json:"action_ids"`
	DecisionIDs []int64    `json:"decision_ids,omitempty"`
	Objects     []string   `json:"objects"`
	Until       *time.Time `json:"until,omitempty"`
	// Released are drop waits that ended at the drop's first window: the
	// drop is still watched over its business cycle.
	Released    []string `json:"released,omitempty"`
	Unavailable string   `json:"unavailable,omitempty"`
	Line        string   `json:"line"`
}

// readWaits puts the live wait of a's change on in (nothing without a
// source).
func (l Loader) readWaits(ctx context.Context, in *Inputs) {
	if l.Waits == nil {
		return
	}
	var targets []string
	if in.Finding != nil && strings.TrimSpace(in.Finding.Object) != "" {
		targets = []string{in.Finding.Object}
	}
	in.Waits, in.WaitsErr = l.Waits.PendingFor(ctx, in.Action.ProposedSQL, targets)
}

// waitOf is the card's wait from its inputs: the changes still in flight
// (a wait past its hard deadline is released and not shown).
func waitOf(in Inputs, now time.Time) *VerificationWait {
	if in.WaitsErr != nil {
		why := in.WaitsErr.Error()
		return &VerificationWait{ActionIDs: []int64{}, Objects: []string{},
			Unavailable: why, Line: "Verification wait: unavailable (" + why + ")"}
	}
	w := &VerificationWait{ActionIDs: []int64{}, Objects: []string{}}
	var names []string
	seen := map[string]bool{}
	for _, p := range in.Waits {
		if p.Expired(now) {
			if p.ReleaseCause() == policy.ReleaseDropFirstWindow {
				w.Released = append(w.Released, waitName(p)+": "+p.ReleaseReason())
			}
			continue
		}
		if p.ActionID > 0 {
			w.ActionIDs = append(w.ActionIDs, p.ActionID)
		} else {
			w.DecisionIDs = append(w.DecisionIDs, p.DecisionID)
		}
		if !seen[p.Object] {
			seen[p.Object] = true
			w.Objects = append(w.Objects, p.Object)
		}
		if w.Until == nil || p.Until.After(*w.Until) {
			until := p.Until
			w.Until = &until
		}
		names = append(names, waitName(p))
	}
	if len(names) == 0 && len(w.Released) == 0 {
		return nil
	}
	w.Line = releasedLine(w.Released)
	if len(names) > 0 {
		w.Line = joinLine(fmt.Sprintf("Awaiting %s (until %s): approving overrides pending %s",
			strings.Join(names, ", "), w.Until.UTC().Format("2006-01-02 15:04 UTC"),
			strings.Join(names, ", ")), w.Line)
	}
	return w
}

// releasedLine says which drop waits ended at their first window.
func releasedLine(released []string) string {
	if len(released) == 0 {
		return ""
	}
	return "Wait released: " + strings.Join(released, "; ") +
		" (the drop's soft-drop monitoring continues over its business cycle)"
}

func joinLine(line, more string) string {
	if more == "" {
		return line
	}
	return line + ". " + more
}

func waitName(p policy.PendingVerification) string {
	if p.ActionID > 0 {
		return fmt.Sprintf("verification of action %d", p.ActionID)
	}
	return fmt.Sprintf("the change authorized by decision %d", p.DecisionID)
}

// waitReason is the wait among the reasons a person decides.
func waitReason(w *VerificationWait) []Reason {
	if w == nil || w.Unavailable != "" {
		return nil
	}
	if len(w.ActionIDs) == 0 && len(w.DecisionIDs) == 0 {
		return []Reason{{Code: "verification_wait_released", Text: w.Line}}
	}
	return []Reason{{Code: string(policy.ReasonAwaitingVerification), Text: w.Line}}
}
