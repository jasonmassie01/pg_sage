package sre

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// The investigator's outcome and the authority rule (roadmap 2.1). The
// model's label is normalized against the graph's actual state (a
// "contest" of an inconclusive graph is a conclusion, a "contest" naming
// the graph's root is agreement). A model-sourced root — a contest of a
// conclusive root or a conclusion of an inconclusive graph — is adopted
// only when the family's earned root authority (#111's held-out rule)
// grants it and the root is an open hypothesis of the graph; otherwise it
// stays advisory (L1). Self-reported confidence is never read. An
// unmodeled cause has no graph node and is always advisory. A root or
// cause that no surviving cited claim supports is downgraded to
// inconclusive.

// Downgrade reasons.
const (
	DowngradeUncited        = "uncited"
	DowngradeInvalidCause   = "invalid_cause"
	DowngradeUnknownNode    = "unknown_node"
	DowngradeInvalidOutcome = "invalid_outcome"
	DowngradeNoRoot         = "no_root"
)

// finalAnswer is the part of the final answer the outcome reads; any
// other field (a "confidence", say) is ignored.
type finalAnswer struct {
	Outcome string          `json:"outcome"`
	Root    string          `json:"root"`
	Cause   *UnmodeledCause `json:"cause"`
}

func decodeFinal(raw json.RawMessage) (finalAnswer, error) {
	var f finalAnswer
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return f, fmt.Errorf("%w: the final answer is not a JSON object", ErrInvalidRequest)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return finalAnswer{}, fmt.Errorf("%w: final answer: %v", ErrInvalidRequest, err)
	}
	return f, nil
}

// resolved is a final answer normalized against the graph.
type resolved struct {
	outcome string
	root    string
	cause   *UnmodeledCause
	reason  string // why it was downgraded to inconclusive
}

func inconclusive(reason string) resolved {
	return resolved{outcome: ModelInconclusive, reason: reason}
}

// resolveOutcome normalizes the model's answer; kept is how many of its
// claims survived the citation filter.
func resolveOutcome(d causal.Diagnosis, f finalAnswer, kept int) resolved {
	graphRoot := graphRootNode(d)
	switch f.Outcome {
	case "inconclusive":
		return inconclusive("")
	case "unmodeled":
		return resolveUnmodeled(f.Cause, kept)
	case "agree", "conclude", "contest":
	default:
		return inconclusive(DowngradeInvalidOutcome)
	}
	root := strings.TrimSpace(f.Root)
	switch {
	case root == "" && f.Outcome == "agree" && graphRoot != "":
		return resolved{outcome: ModelAgreed, root: graphRoot}
	case root == "":
		return inconclusive(DowngradeNoRoot)
	}
	if _, ok := causal.NodeByID(causal.NodeID(root)); !ok {
		return inconclusive(DowngradeUnknownNode)
	}
	switch {
	case root == graphRoot:
		return resolved{outcome: ModelAgreed, root: root}
	case kept == 0:
		return inconclusive(DowngradeUncited)
	case graphRoot != "":
		return resolved{outcome: ModelContested, root: root}
	}
	return resolved{outcome: ModelConcluded, root: root}
}

func resolveUnmodeled(c *UnmodeledCause, kept int) resolved {
	if kept == 0 {
		return inconclusive(DowngradeUncited)
	}
	if c == nil {
		return inconclusive(DowngradeInvalidCause)
	}
	cause := &UnmodeledCause{Label: strings.Join(strings.Fields(c.Label), " "),
		Mechanism: strings.Join(strings.Fields(c.Mechanism), " ")}
	if cause.validate() != nil || utf8.RuneCountInString(cause.Label) > maxCauseLabelRunes {
		return inconclusive(DowngradeInvalidCause)
	}
	return resolved{outcome: ModelUnmodeled, cause: cause}
}

func graphRootNode(d causal.Diagnosis) string {
	if d.Conclusive && d.Root != nil {
		return string(d.Root.Node)
	}
	return ""
}

// applyAuthority applies a resolved outcome under the grant: it returns
// the diagnosis to conclude, the model conclusion and, for a contest of
// an open hypothesis, the contest record. d is not modified.
func applyAuthority(d causal.Diagnosis, r resolved, g RootGrant) (causal.Diagnosis,
	*ModelConclusion, *ModelContest) {
	mc := &ModelConclusion{Label: ModelConclusionLabel, Outcome: r.outcome, Root: r.root,
		Cause: r.cause, GraphRoot: graphRootNode(d), Authority: ContestAdvisory,
		Reason: outcomeReason(r)}
	switch r.outcome {
	case ModelContested:
		return contestOutcome(d, mc, g)
	case ModelConcluded:
		return concludeOutcome(d, mc, g)
	}
	return d, mc, nil
}

func outcomeReason(r resolved) string {
	switch r.outcome {
	case ModelAgreed:
		return "the model agrees with the causal graph's root"
	case ModelUnmodeled:
		return "an unmodeled cause has no graph node, so it stays advisory (L1)"
	case ModelInconclusive:
		if r.reason != "" {
			return "the model did not conclude (" + r.reason + ")"
		}
		return "the model did not conclude"
	}
	return "no reason given"
}

const notOpenReason = "the model's root is not an open hypothesis of the graph, so it " +
	"stays advisory (L1)"

func grantReason(g RootGrant) string {
	if r := strings.Join(strings.Fields(g.Reason), " "); r != "" {
		return truncateRunes(r, maxContestReasonRunes)
	}
	return "no reason given"
}

func contestOutcome(d causal.Diagnosis, mc *ModelConclusion, g RootGrant) (
	causal.Diagnosis, *ModelConclusion, *ModelContest) {
	mc.Reason = grantReason(g)
	if !isOpen(d, mc.Root) {
		mc.Reason = notOpenReason
		return d, mc, nil
	}
	contest := &ModelContest{Label: ModelContestLabel, GraphRoot: mc.GraphRoot,
		ModelRoot: mc.Root, Authority: ContestAdvisory, Reason: mc.Reason}
	if !g.Granted {
		return d, mc, contest
	}
	adopted, ok := adoptRoot(d, mc.Root)
	if !ok {
		mc.Reason, contest.Reason = notOpenReason, notOpenReason
		return d, mc, contest
	}
	mc.Authority, contest.Authority = ContestAdopted, ContestAdopted
	return adopted, mc, contest
}

func concludeOutcome(d causal.Diagnosis, mc *ModelConclusion, g RootGrant) (
	causal.Diagnosis, *ModelConclusion, *ModelContest) {
	mc.Reason = grantReason(g)
	if !g.Granted {
		if !isOpen(d, mc.Root) {
			mc.Reason = notOpenReason
		}
		return d, mc, nil
	}
	adopted, ok := concludeOn(d, mc.Root)
	if !ok {
		mc.Reason = notOpenReason
		return d, mc, nil
	}
	mc.Authority = ContestAdopted
	return adopted, mc, nil
}

// isOpen reports whether node is a hypothesis of d that is not ruled out.
func isOpen(d causal.Diagnosis, node string) bool {
	for _, n := range openNodes(d) {
		if n == node {
			return true
		}
	}
	return false
}

// concludeOn roots an inconclusive diagnosis on node, an open hypothesis
// of it. d is not modified; false when node cannot be the root.
func concludeOn(d causal.Diagnosis, node string) (causal.Diagnosis, bool) {
	if d.Conclusive || node == "" {
		return d, false
	}
	pick := func(hs []causal.Hypothesis) ([]causal.Hypothesis, *causal.Hypothesis) {
		out := make([]causal.Hypothesis, 0, len(hs))
		var found *causal.Hypothesis
		for i := range hs {
			if string(hs[i].Node) == node && found == nil {
				h := hs[i]
				found = &h
				continue
			}
			out = append(out, hs[i])
		}
		return out, found
	}
	alternatives, root := pick(d.Alternatives)
	contributing := d.Contributing
	if root == nil {
		alternatives = d.Alternatives
		contributing, root = pick(d.Contributing)
	}
	if root == nil {
		return d, false
	}
	out := d
	root.Status = causal.StatusRoot
	out.Root, out.Conclusive = root, true
	out.Alternatives, out.Contributing = alternatives, contributing
	out.Reason = "the causal graph was inconclusive (" + d.Reason + "); the model's " +
		"conclusion was adopted under the family's earned root authority"
	return out, true
}

// authorityFamily is the family whose root authority decides a model
// root: the root node's own family (an SLO or operator triage spans
// families), else the diagnosis' family.
func authorityFamily(d causal.Diagnosis, node string) string {
	if n, ok := causal.NodeByID(causal.NodeID(node)); ok {
		return string(n.Family)
	}
	return string(d.Family)
}
