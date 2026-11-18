package candidate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Validate refuses, with ErrInvalid naming the candidate: an id that is
// not NewID of its parts, an action type that is not a registered
// contract, advice or operator, no evidence ids, an evidence id that does
// not resolve in pack, a registered action with no forward SQL, and a
// registered action whose SQL classifies (executor.ClassifySQL) to another
// action type. A type the classifier does not cover by SQL is checked by
// its contract only. A step changeplan.NeedsOwnApproval says is never
// automatic is moved to ApprovalRequired, with the reason; an empty
// approval fails closed the same way. A nil pack is an error.
func (c *Candidate) Validate(pack *workpack.Pack) error {
	if pack == nil {
		return fmt.Errorf("validate candidate %s: no evidence pack", c.name())
	}
	if err := c.checkShape(); err != nil {
		return err
	}
	for _, id := range c.EvidenceIDs {
		if _, err := workpack.Resolve(pack, id); err != nil {
			return c.invalid("evidence id %q does not resolve in the pack: %v", id, err)
		}
	}
	if err := c.checkAction(); err != nil {
		return err
	}
	c.gateApproval()
	return nil
}

// name is how errors name the candidate: its target and short id.
func (c *Candidate) name() string {
	id := c.ID
	if len(id) > 12 {
		id = id[:12]
	}
	return fmt.Sprintf("%s (%s %s, id %s)", c.Target, c.Generator, c.Kind, id)
}

func (c *Candidate) invalid(format string, args ...any) error {
	return fmt.Errorf("%w %s: %s", ErrInvalid, c.name(), fmt.Sprintf(format, args...))
}

// checkShape checks the id, the evidence list and the approval word.
func (c *Candidate) checkShape() error {
	if c.ID != NewID(c.Generator, c.Kind, c.Target, c.ForwardSQL) {
		return c.invalid("the id is not the hash of its generator, kind, target and SQL")
	}
	if len(c.EvidenceIDs) == 0 {
		return c.invalid("no evidence ids: a candidate must cite the pack")
	}
	switch c.Approval {
	case "", ApprovalAutomatic, ApprovalRequired:
		return nil
	}
	return c.invalid("approval %q is not %s or %s", c.Approval, ApprovalAutomatic,
		ApprovalRequired)
}

// checkAction checks the action type and that its SQL agrees with it.
func (c *Candidate) checkAction() error {
	switch c.ActionType {
	case ActionAdvice, ActionOperator:
		return nil
	}
	if _, ok := executor.ContractForActionType(c.ActionType); !ok {
		return c.invalid("action type %q is not a registered executor contract, %s or %s",
			c.ActionType, ActionAdvice, ActionOperator)
	}
	if strings.TrimSpace(c.ForwardSQL) == "" {
		return c.invalid("empty forward SQL for registered action %s", c.ActionType)
	}
	if got, _ := executor.ClassifySQL(c.ForwardSQL); got != "" && got != c.ActionType {
		return c.invalid("the forward SQL classifies as %s, not as its action type %s", got,
			c.ActionType)
	}
	return nil
}

// gateApproval moves a never-automatic step to approval_required.
func (c *Candidate) gateApproval() {
	if c.ActionType == ActionAdvice || c.ActionType == ActionOperator {
		c.Approval = ApprovalRequired
		return
	}
	needs, why := changeplan.NeedsOwnApproval(changeplan.Step{Kind: changeplan.KindAction,
		ActionType: c.ActionType, ProposedSQL: c.ForwardSQL, RollbackSQL: c.RollbackSQL}, nil)
	switch {
	case needs:
		c.Approval, c.ApprovalReason = ApprovalRequired, why
	case c.Approval == "":
		c.Approval, c.ApprovalReason = ApprovalRequired, "no approval stated (fail closed)"
	}
}

// ValidateAll validates each candidate, joining every refusal.
func ValidateAll(pack *workpack.Pack, cands []Candidate) error {
	var errs []error
	for i := range cands {
		if err := cands[i].Validate(pack); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
