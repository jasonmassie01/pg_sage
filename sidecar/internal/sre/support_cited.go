package sre

import (
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The Support-evidence rule (sre-03 Phase 2): a model contest or
// conclusion names a root, and Phase 1 kept it on any surviving cited
// claim, about anything. With the rule on, at least one kept claim must
// cite evidence the causal graph itself records as support for that
// root (Hypothesis.Support). A claim about connection counts cannot
// carry a lock_blocking root.

// DowngradeUnsupported is the reason a contest or conclusion is
// inconclusive when no kept claim cites the root's support.
const DowngradeUnsupported = "unsupported_root"

// supportCited reports whether a claim of n cites stored, non-snapshot
// evidence that some hypothesis of d whose node is root lists as
// support. Facts bind evidence by the stored id's canonical string form
// (observations, plan.go), so the claim's UUID is compared as that
// string. Like liveClaims, the detection snapshot never counts.
func supportCited(d causal.Diagnosis, root string, n *Narrative, stored []Evidence) bool {
	if n == nil || root == "" {
		return false
	}
	support := rootSupport(d, root)
	if len(support) == 0 {
		return false
	}
	citable := map[UUID]bool{}
	for _, e := range stored {
		citable[e.ID] = probes.ID(e.ProbeID) != IncidentSnapshotProbeID
	}
	for _, c := range n.Claims {
		for _, id := range c.EvidenceIDs {
			if citable[id] && support[string(id)] {
				return true
			}
		}
	}
	return false
}

// rootSupport is the evidence ids every hypothesis of d on node lists as
// support, whatever its status.
func rootSupport(d causal.Diagnosis, node string) map[string]bool {
	out := map[string]bool{}
	add := func(h causal.Hypothesis) {
		if string(h.Node) != node {
			return
		}
		for _, f := range h.Support {
			if f.EvidenceID != "" {
				out[f.EvidenceID] = true
			}
		}
	}
	if d.Root != nil {
		add(*d.Root)
	}
	for _, hs := range [][]causal.Hypothesis{d.Contributing, d.Alternatives, d.RuledOut} {
		for _, h := range hs {
			add(h)
		}
	}
	return out
}

// resolveSupported resolves the model's answer under the citation rules:
// Phase 1 (an outcome needs a kept claim citing live evidence) and, when
// require is set, the Support-evidence rule for a contest or conclusion.
// Agreement and an unmodeled cause (no graph node to support) are never
// affected; an earlier downgrade keeps its own reason.
func resolveSupported(d causal.Diagnosis, f finalAnswer, n *Narrative, stored []Evidence,
	require bool) resolved {
	r := resolveOutcome(d, f, liveClaims(n, stored))
	if !require || (r.outcome != ModelContested && r.outcome != ModelConcluded) {
		return r
	}
	if !supportCited(d, r.root, n, stored) {
		return inconclusive(DowngradeUnsupported)
	}
	return r
}
