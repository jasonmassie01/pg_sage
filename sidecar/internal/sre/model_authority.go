package sre

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// Model-root authority (roadmap 2.4). The blanket rule "the model may
// never change a graph root" is retired for a measured, per-family one:
// when the graph is conclusive and the model ranks another open
// hypothesis first, the coordinator asks the RootAuthority (the trust
// ledger, which reads the held-out bench) whether the model may override
// the graph's root for the investigation's family. Granted, the model's
// root is adopted and the graph's root is kept as a contributing factor;
// otherwise, or when the authority cannot be read, the graph's root
// stands and the contest is stored beside it as advisory (L1). Either way
// the contest is recorded (model_disagreed, Summary.ModelContest).

// RootAuthority decides, per incident family, whether the model may
// override a conclusive graph root.
type RootAuthority interface {
	ModelRootAuthority(ctx context.Context, family string) (RootGrant, error)
}

// RootGrant is a family's authority and why.
type RootGrant struct {
	Granted bool
	Reason  string
}

// Contest authorities.
const (
	// ContestAdvisory: the graph's root stands; the model's is shown.
	ContestAdvisory = "advisory"
	// ContestAdopted: the model's root replaced the graph's.
	ContestAdopted = "adopted"
	// ModelContestLabel labels the stored contest.
	ModelContestLabel = "model root contest"

	maxContestReasonRunes = 500
	// rootAuthorityTimeout bounds one authority lookup.
	rootAuthorityTimeout = 5 * time.Second
)

// ModelContest is a model ranking that contested a conclusive graph root.
type ModelContest struct {
	Label     string `json:"label"`
	GraphRoot string `json:"graph_root"`
	ModelRoot string `json:"model_root"`
	Authority string `json:"authority"`
	Reason    string `json:"reason"`
}

// rootAuthority asks the authority for family, bounded in time. A missing
// authority or a failed lookup is advisory, logged, never fatal.
func (c *Coordinator) rootAuthority(ctx context.Context, inv Investigation,
	family string) RootGrant {
	if c.authority == nil {
		return RootGrant{Reason: "no root authority is configured, so model roots stay " +
			"advisory (L1)"}
	}
	ctx, cancel := context.WithTimeout(ctx, rootAuthorityTimeout)
	defer cancel()
	g, err := c.authority.ModelRootAuthority(ctx, family)
	if err != nil {
		c.logFn("WARN", "sre: investigation %s: reading the model root authority for %s "+
			"failed, the model's root stays advisory: %v", inv.ID, family, err)
		return RootGrant{Reason: "the root authority was unavailable, so the model's root " +
			"stays advisory (L1)"}
	}
	g.Reason = truncateRunes(RedactText(strings.TrimSpace(g.Reason)), maxContestReasonRunes)
	if g.Reason == "" {
		g.Reason = "no reason given"
	}
	return g
}

// adoptRoot re-roots a conclusive diagnosis on node, an open (contributing
// or unproven) hypothesis; the graph's root becomes a contributing
// factor. d is not modified; false when node cannot be adopted.
func adoptRoot(d causal.Diagnosis, node string) (causal.Diagnosis, bool) {
	if !d.Conclusive || d.Root == nil || node == "" || string(d.Root.Node) == node {
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
	contributing, fromC := pick(d.Contributing)
	alternatives, fromA := pick(d.Alternatives)
	adopted := fromC
	if adopted == nil {
		adopted = fromA
	}
	if adopted == nil {
		return d, false
	}
	old := *d.Root
	old.Status, adopted.Status = causal.StatusContributing, causal.StatusRoot
	out := d
	out.Root = adopted
	out.Contributing = append([]causal.Hypothesis{old}, contributing...)
	out.Alternatives = alternatives
	return out, true
}

// validate checks a stored contest against the hypotheses and the root.
func (m *ModelContest) validate(hs []HypothesisRecord, root string, concluded bool) error {
	if m == nil {
		return nil
	}
	open := map[string]bool{}
	for _, h := range hs {
		if h.Status != HypothesisRuledOut {
			open[h.Node] = true
		}
	}
	reason := strings.TrimSpace(m.Reason)
	switch {
	case m.Label != ModelContestLabel:
		return fmt.Errorf("%w: model contest must carry its label", ErrInvalidRequest)
	case !concluded:
		return fmt.Errorf("%w: only a concluded investigation has a contested root",
			ErrInvalidRequest)
	case m.GraphRoot == m.ModelRoot || !open[m.GraphRoot] || !open[m.ModelRoot]:
		return fmt.Errorf("%w: model contest roots must be two open hypotheses",
			ErrInvalidRequest)
	case reason == "" || len([]rune(m.Reason)) > maxContestReasonRunes:
		return fmt.Errorf("%w: model contest reason", ErrInvalidRequest)
	case m.Authority == ContestAdvisory && root != m.GraphRoot,
		m.Authority == ContestAdopted && root != m.ModelRoot:
		return fmt.Errorf("%w: a %s contest does not match the root %q", ErrInvalidRequest,
			m.Authority, root)
	case m.Authority != ContestAdvisory && m.Authority != ContestAdopted:
		return fmt.Errorf("%w: model contest authority %q", ErrInvalidRequest, m.Authority)
	}
	return nil
}
