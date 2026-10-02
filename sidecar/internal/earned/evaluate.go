package earned

import "context"

// Why Evaluate did not propose a pair (2026-10-02 roadmap Phase 1.1:
// "evaluate" must explain itself instead of returning an empty list).
const (
	// NotProposedEvidence: the next level's checks are not all met.
	NotProposedEvidence = "evidence_not_met"
	// NotProposedPending: a proposal for the pair already waits for a human.
	NotProposedPending = "pending"
	// NotProposedAtCap: the pair is at its class cap (irreversible: L1).
	NotProposedAtCap = "at_cap"
)

// NotProposed is one applicable pair Evaluate did not propose, and why.
type NotProposed struct {
	Family  Family      `json:"family"`
	Class   ActionClass `json:"class"`
	Granted Level       `json:"granted"`
	// Target is the next level, below the cap.
	Target *Level `json:"target,omitempty"`
	Reason string `json:"reason"`
	// Pending is the waiting proposal's id (reason pending).
	Pending string `json:"pending,omitempty"`
	// Unmet are the next level's unmet checks, each with its instruction
	// (reason evidence_not_met).
	Unmet []Check `json:"unmet,omitempty"`
}

// Evaluation is one evaluation of every applicable pair of the database.
type Evaluation struct {
	Created     []Proposal    `json:"created"`
	NotProposed []NotProposed `json:"not_proposed"`
}

// Evaluate expires stale proposals, then proposes one level up for every
// applicable pair whose evidence supports it, below its cap and without a
// pending proposal, and explains every pair it did not propose. It never
// changes a level.
func (s *Service) Evaluate(ctx context.Context) (Evaluation, error) {
	e := Evaluation{Created: []Proposal{}, NotProposed: []NotProposed{}}
	if err := s.expire(ctx); err != nil {
		return e, err
	}
	pending, err := s.store.listProposals(ctx, StatusPending, 200)
	if err != nil {
		return e, err
	}
	waiting := map[pairKey]string{}
	for _, p := range pending {
		waiting[pairKey{p.Family, p.Class}] = p.ID
	}
	for _, f := range Families() {
		for _, c := range ApplicableClasses(f) {
			if err := s.evaluateOne(ctx, f, c, waiting, &e); err != nil {
				return e, err
			}
		}
	}
	return e, nil
}

func (s *Service) evaluateOne(ctx context.Context, f Family, c ActionClass,
	waiting map[pairKey]string, e *Evaluation) error {
	st, err := s.Granted(ctx, f, c)
	if err != nil {
		return err
	}
	np := NotProposed{Family: f, Class: c, Granted: st.Level}
	target := st.Level + 1
	if target > CapFor(c) || !target.Grantable() {
		np.Reason = NotProposedAtCap
		e.NotProposed = append(e.NotProposed, np)
		return nil
	}
	np.Target = &target
	if id, ok := waiting[pairKey{f, c}]; ok {
		np.Reason, np.Pending = NotProposedPending, id
		e.NotProposed = append(e.NotProposed, np)
		return nil
	}
	ev, err := s.Evidence(ctx, f, c)
	if err != nil {
		return err
	}
	a := Assess(s.cfg.Thresholds, target, ev)
	if !a.Met {
		np.Reason = NotProposedEvidence
		for _, check := range a.Checks {
			if !check.Met {
				np.Unmet = append(np.Unmet, check)
			}
		}
		e.NotProposed = append(e.NotProposed, np)
		return nil
	}
	p, created, err := s.createProposal(ctx, st, target, ev, a)
	if err != nil {
		return err
	}
	if !created {
		// Another evaluation proposed the pair concurrently.
		np.Reason = NotProposedPending
		e.NotProposed = append(e.NotProposed, np)
		return nil
	}
	e.Created = append(e.Created, p)
	return nil
}
