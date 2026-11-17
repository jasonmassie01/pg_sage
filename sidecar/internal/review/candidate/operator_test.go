package candidate

import (
	"errors"
	"strings"
	"testing"
)

// operatorCandidate is a valid restart-bound operator step on t_a's pack
// (review-02b): what the operator runs, its revert and the landing.
func operatorCandidate() Candidate {
	c := Candidate{Generator: "config", Dimension: "configuration", Kind: "shared_buffers",
		Target: "shared_buffers", ActionType: ActionOperator, EvidenceIDs: []string{factID},
		Reason: "shared_buffers is 3% of memory", Approval: ApprovalAutomatic,
		Operator: &Operator{Command: "ALTER SYSTEM SET shared_buffers = '4GB'; then restart",
			Revert:  "ALTER SYSTEM RESET shared_buffers; then restart",
			Setting: "shared_buffers", Value: "4GB", RebootRequired: true},
		Prediction: Prediction{Metric: "shared_buffers", Direction: "up", Tier: TierNone,
			Source: "rule"}}
	c.ID = NewID(c.Generator, c.Kind, c.Target, c.IDBody())
	return c
}

func TestIDBody_IsTheSQLOrTheOperatorCommand(t *testing.T) {
	if d := dropCandidate(); d.IDBody() != dropSQL {
		t.Fatalf("a step's id body is %q, want its forward SQL", d.IDBody())
	}
	op := operatorCandidate()
	if op.IDBody() != op.Operator.Command {
		t.Fatalf("an operator candidate's id body is %q, want its command", op.IDBody())
	}
	other := operatorCandidate()
	other.Operator.Command = strings.Replace(other.Operator.Command, "4GB", "8GB", 1)
	if NewID(op.Generator, op.Kind, op.Target, op.IDBody()) ==
		NewID(other.Generator, other.Kind, other.Target, other.IDBody()) {
		t.Fatal("two operator values share one id")
	}
}

func TestValidate_AcceptsAnOperatorStepAlwaysApprovalRequired(t *testing.T) {
	c := operatorCandidate()
	if err := c.Validate(testPack(t)); err != nil {
		t.Fatalf("valid operator candidate refused: %v", err)
	}
	if c.Approval != ApprovalRequired {
		t.Fatalf("an operator step's approval is %q, want %s", c.Approval, ApprovalRequired)
	}
}

func TestValidate_RefusesMalformedOperatorFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Candidate)
		want   string
	}{
		{"no operator block", func(c *Candidate) { c.Operator = nil },
			"no operator command"},
		{"blank command", func(c *Candidate) { c.Operator.Command = " " },
			"no operator command"},
		{"no landing setting", func(c *Candidate) { c.Operator.Setting = "" }, "landing"},
		{"no landing value", func(c *Candidate) { c.Operator.Value = "" }, "landing"},
		{"SQL for pg_sage to run", func(c *Candidate) {
			c.ForwardSQL = "ALTER SYSTEM SET shared_buffers = '4GB'"
		}, "pg_sage runs nothing"},
		{"rollback SQL", func(c *Candidate) { c.RollbackSQL = "ALTER SYSTEM RESET x" },
			"pg_sage runs nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := operatorCandidate()
			tc.mutate(&c)
			c.ID = NewID(c.Generator, c.Kind, c.Target, c.IDBody())
			err := c.Validate(testPack(t))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "shared_buffers") {
				t.Fatalf("err = %v, want ErrInvalid naming shared_buffers and %q", err,
					tc.want)
			}
		})
	}
}

func TestValidate_RefusesOperatorFieldsOnAStepOrAdvice(t *testing.T) {
	for _, action := range []string{"drop_unused_index", ActionAdvice} {
		c := dropCandidate()
		c.ActionType = action
		if action == ActionAdvice {
			c.ForwardSQL, c.RollbackSQL = "", ""
		}
		c.Operator = &Operator{Command: "run me", Setting: "work_mem", Value: "8MB"}
		c.ID = NewID(c.Generator, c.Kind, c.Target, c.IDBody())
		err := c.Validate(testPack(t))
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "only an operator") {
			t.Fatalf("%s with operator fields: err = %v, want ErrInvalid", action, err)
		}
	}
}
