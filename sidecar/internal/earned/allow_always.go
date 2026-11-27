package earned

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Allow always (ux-07a): an approval card offers "Approve, and stop
// asking for <class> on <database>" when its class may take the L2 to L3
// step now. Pressing it approves the action, then the class's L3
// promotion through the ordinary proposal and Approve: a person grants
// it, the level never skips, and it decays and demotes like any earned
// level.

// AllowAlways reports whether a card of row's class may offer
// allow-always under ceil, and why not when it may not: the class is
// reversible and stands at L2, at L3 the gate would let it act, the L3
// observation floor has passed, and either an L2 to L3 proposal waits or
// the approval history qualifies.
func AllowAlways(row TrustRow, ceil Ceiling) (bool, string) {
	switch {
	case row.Reversibility != Reversible:
		return false, fmt.Sprintf("%s cannot be undone, so it always asks first", row.Class)
	case row.Level != L2 || (row.Effective != nil && *row.Effective != L2):
		return false, fmt.Sprintf("%s stands at %s; stop asking is the L2 to L3 step",
			row.Class, rowLevel(row))
	case row.Cap < L3:
		return false, fmt.Sprintf("%s is capped at %s", row.Class, row.Cap)
	}
	if !floorPassed(row, ceil) {
		return false, "the trust ramp floor for L3 has not passed"
	}
	at := row
	at.Level, at.Effective, at.Downgrades, at.MajorCap = L3, nil, nil, nil
	if m := ModeOf(at, ceil); m.Name != ModeActsThenTells && m.Name != ModeActsInWindow {
		why := m.Name.Sentence()
		if len(m.Because) > 0 {
			why += ": " + m.Because[0]
		}
		return false, "at L3 it would still not run unattended (" + why + ")"
	}
	if p := row.Pending; p != nil {
		if p.From == L2 && p.To == L3 {
			return true, "a promotion to L3 waits for a person"
		}
		return false, fmt.Sprintf("a promotion to %s waits", p.To)
	}
	h := row.ApprovalHistory
	if h == nil || !h.Qualifies {
		return false, approvalsShort(h)
	}
	return true, fmt.Sprintf("%d person approvals with no rollback", h.Approvals)
}

// approvalsShort says how far the approval history is from asking.
func approvalsShort(h *ApprovalHistory) string {
	switch {
	case h == nil:
		return "no approval history for this class"
	case h.Required <= 0:
		return "approval asks are off (trust.ask_after_approvals is 0)"
	}
	return fmt.Sprintf("%d of %d person approvals with no rollback", h.Approvals, h.Required)
}

// floorPassed reads the L3 observation floor from the row's next-level
// checks (a self-initiated class), else from the ceiling's ramp.
func floorPassed(row TrustRow, ceil Ceiling) bool {
	if row.Next != nil {
		for _, c := range row.Next.Checks {
			if c.Name == "observation_floor" {
				return c.Met
			}
		}
	}
	if ceil.RampStart.IsZero() {
		return false
	}
	now := ceil.Now
	if now.IsZero() {
		now = time.Now()
	}
	floor := RampFloor{Start: ceil.RampStart, Safe: ceil.SafeRampAge,
		Moderate: ceil.ModerateRampAge}
	return now.Sub(ceil.RampStart) >= floor.For(L3, row.Class)
}

// CheckHumanApprover refuses an approver that is not a person: pg_sage,
// a system actor or an agent (MCP token or stdio) never grants autonomy.
func CheckHumanApprover(actor string) error {
	if !humanActor(actor) {
		return fmt.Errorf("%w: approver %q", ErrHumanApprovalRequired, actor)
	}
	return nil
}

// AllowAlways approves the pair's L3 promotion as approver with note:
// the waiting L2 to L3 proposal, else one it proposes from the approval
// history. A proposal it created that loses a race to another grant is
// superseded, so no stale promotion waits.
func (s *Service) AllowAlways(ctx context.Context, f Family, c ActionClass, approver,
	note string) (State, error) {
	if err := CheckHumanApprover(approver); err != nil {
		return State{}, err
	}
	if len(note) > 2000 {
		return State{}, fmt.Errorf("%w: note longer than 2000 characters", ErrInvalidRequest)
	}
	p, created, err := s.allowAlwaysProposal(ctx, f, c)
	if err != nil {
		return State{}, err
	}
	st, err := s.Approve(ctx, p.ID, approver, note)
	if err != nil && created && errors.Is(err, ErrConflict) {
		if _, cerr := s.store.closePending(ctx, s.store.pool, f, c, StatusSuperseded,
			ActorPgSage, "superseded: the level moved before it was approved",
			s.now()); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}
	return st, err
}

// allowAlwaysProposal is the pair's waiting L2 to L3 proposal, else one
// proposed now; created reports that this call stored it.
func (s *Service) allowAlwaysProposal(ctx context.Context, f Family, c ActionClass) (
	Proposal, bool, error) {
	if p, err := s.pendingFor(ctx, f, c); err != nil || p != nil {
		if err == nil && (p.From != L2 || p.To != L3) {
			err = fmt.Errorf("%w: the waiting proposal is %s to %s; stop asking is the L2 "+
				"to L3 step", ErrConflict, p.From, p.To)
		}
		if err != nil {
			return Proposal{}, false, err
		}
		return *p, false, nil
	}
	return s.ProposeFromApprovals(ctx, f, c)
}

// pendingFor is the pair's waiting proposal, nil when none waits.
func (s *Service) pendingFor(ctx context.Context, f Family, c ActionClass) (*Proposal,
	error) {
	ps, err := s.PendingProposals(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		if ps[i].Family == f && ps[i].Class == c {
			return &ps[i], nil
		}
	}
	return nil, nil
}

// ProposeFromApprovals proposes a self-initiated pair standing at L2 for
// L3 on its approval history; created is false when another call stored
// the pair's waiting proposal first (that one is returned).
func (s *Service) ProposeFromApprovals(ctx context.Context, f Family, c ActionClass) (
	Proposal, bool, error) {
	if !IsSelfInitiated(f) || !Applicable(f, c) {
		return Proposal{}, false, fmt.Errorf("%w: %s/%s has no L3 proposal waiting; an "+
			"incident class earns L3 from its incident evidence, not from approvals",
			ErrEvidenceNotMet, f, c)
	}
	st, err := s.Granted(ctx, f, c)
	if err != nil {
		return Proposal{}, false, err
	}
	if st.Level != L2 {
		return Proposal{}, false, fmt.Errorf("%w: %s/%s stands at %s; stop asking is the "+
			"L2 to L3 step", ErrConflict, f, c, st.Level)
	}
	ev, err := s.decisionEvidence(ctx, f, c)
	if err != nil {
		return Proposal{}, false, err
	}
	a := AssessApprovalHistory(L3, ev)
	if !a.Met {
		return Proposal{}, false, &EvidenceNotMetError{Assessment: a}
	}
	p, created, err := s.createProposal(ctx, st, L3, ev, a, EvidenceKindApprovalHistory)
	if err != nil || created {
		return p, created, err
	}
	waiting, err := s.pendingFor(ctx, f, c)
	if err == nil && waiting == nil {
		err = fmt.Errorf("%w: %s/%s's proposal was decided concurrently", ErrConflict, f, c)
	}
	if err != nil {
		return Proposal{}, false, err
	}
	return *waiting, false, nil
}

// AllowAlwaysLabel is the card's second affirmative button.
func AllowAlwaysLabel(c ActionClass, database string) string {
	return strings.TrimSpace(fmt.Sprintf("Approve, and stop asking for %s on %s", c,
		database))
}
