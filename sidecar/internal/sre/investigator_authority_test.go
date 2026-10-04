package sre

import (
	"errors"
	"strings"
	"testing"
)

// Authority edge cases pinned by mutation testing: a contest that names
// a node the graph ruled out (or does not have) is no disagreement at
// all, granted or not, and an unmodeled cause can never carry adopted
// authority, whatever the root it is checked against.

func TestApplyAuthority_ContestOfANonOpenNodeIsNoDisagreement(t *testing.T) {
	d := conclusiveLock()
	for _, node := range []string{"prepared_xact_holder", "connection_leak"} {
		r := resolveOutcome(d, final("contest", node), 1)
		out, mc, contest := applyAuthority(d, r, RootGrant{Reason: "not earned"})
		if contest != nil {
			t.Errorf("%s: contest %+v recorded for a node that is not open", node, contest)
		}
		if out.Root.Node != d.Root.Node || mc.Authority != ContestAdvisory ||
			!strings.Contains(mc.Reason, "not an open hypothesis") {
			t.Errorf("%s: root %s, conclusion %+v", node, out.Root.Node, mc)
		}
	}
}

func TestModelConclusion_AdoptedUnmodeledFailsItsShape(t *testing.T) {
	mc := ModelConclusion{Label: ModelConclusionLabel, Outcome: ModelUnmodeled,
		Authority: ContestAdopted, Reason: "why",
		Cause: &UnmodeledCause{Label: "deploy", Mechanism: "held a transaction"}}
	if err := mc.validateShape(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("validateShape = %v, want an adopted unmodeled cause refused", err)
	}
	mc.Authority = ContestAdvisory
	if err := mc.validateShape(); err != nil {
		t.Fatalf("an advisory unmodeled cause is valid: %v", err)
	}
}
