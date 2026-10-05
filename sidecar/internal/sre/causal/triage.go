package causal

import "fmt"

// maxTriageObserved bounds the observed facts a triage keeps.
const maxTriageObserved = 8

// DiagnoseSLOBurn triages a burning SLO across the database mechanisms
// the evidence can show: lock blocking, connection pressure and plan
// regressions. The most confident conclusive mechanism is the root;
// other conclusive roots stay unproven alternatives. With none, the
// burn is inconclusive and the reason says the cause may be outside
// PostgreSQL. Nothing is invented: every hypothesis comes from a family
// matcher and its evidence.
func DiagnoseSLOBurn(obs []Observation, subject string) Diagnosis {
	return triage(obs, subject, FamilySLO, fmt.Sprintf("%s is burning, but the lock, "+
		"connection and plan evidence shows no database mechanism: the cause may be "+
		"outside PostgreSQL", subject))
}

// FamilyOperator is an operator-started triage ("Investigate now").
const FamilyOperator Family = "operator"

// DiagnoseOperator triages an operator-started investigation across the
// same mechanisms as an SLO burn, without claiming an SLO is burning.
func DiagnoseOperator(obs []Observation, subject string) Diagnosis {
	what := "the reported problem"
	if subject != "" {
		what = subject
	}
	return triage(obs, subject, FamilyOperator, fmt.Sprintf("the lock, connection and "+
		"plan evidence shows no database mechanism for %s: the cause may be outside "+
		"PostgreSQL or in a mechanism the graph does not model", what))
}

// triage picks the most confident conclusive mechanism of the lock,
// connection and plan families as the root; noneReason is the reason
// when none concludes.
func triage(obs []Observation, subject string, family Family, noneReason string) Diagnosis {
	candidates := []Diagnosis{DiagnoseLock(obs, nil), DiagnoseConnections(obs)}
	candidates = append(candidates, DiagnosePlan(obs)...)
	best := -1
	for i, c := range candidates {
		if c.Conclusive && c.Root != nil &&
			(best < 0 || c.Root.Confidence > candidates[best].Root.Confidence) {
			best = i
		}
	}
	d := Diagnosis{Family: family, GraphVersion: GraphVersion, Subject: subject}
	for i, c := range candidates {
		d.mergeEvidence(c)
		if i == best {
			continue
		}
		if c.Conclusive && c.Root != nil {
			alt := *c.Root
			alt.Status = StatusAlternative
			d.Alternatives = append(d.Alternatives, alt)
		}
		d.Alternatives = append(d.Alternatives, c.Alternatives...)
		d.Alternatives = append(d.Alternatives, c.Contributing...)
		d.RuledOut = append(d.RuledOut, c.RuledOut...)
	}
	if best < 0 {
		d.Reason = noneReason
		return d
	}
	b := candidates[best]
	d.Root, d.Conclusive = b.Root, true
	d.Contributing = append(d.Contributing, b.Contributing...)
	d.Alternatives = append(append([]Hypothesis(nil), b.Alternatives...), d.Alternatives...)
	d.RuledOut = append(append([]Hypothesis(nil), b.RuledOut...), d.RuledOut...)
	d.Ratio = b.Ratio
	return d
}

// mergeEvidence keeps a candidate's missing evidence (once per probe)
// and its observed facts (bounded).
func (d *Diagnosis) mergeEvidence(c Diagnosis) {
	for _, m := range c.Missing {
		dup := false
		for _, have := range d.Missing {
			dup = dup || (have.ProbeID == m.ProbeID && have.Reason == m.Reason)
		}
		if !dup {
			d.Missing = append(d.Missing, m)
		}
	}
	for _, f := range c.Observed {
		if len(d.Observed) < maxTriageObserved {
			d.Observed = append(d.Observed, f)
		}
	}
}
