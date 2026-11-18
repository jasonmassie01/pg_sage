package whynot

import (
	"fmt"
	"time"
)

// decisionExecute is the executor's verdict for a change it would run.
const decisionExecute = "execute"

// verdictLink is the link a verdict lands on and whether it lets the
// change run. A reason no mapping names is a policy failure as written.
func verdictLink(v Verdict) (reasonMapping, bool) {
	if m, ok := mappingFor(v.Reason); ok {
		if m.allows || v.Decision != decisionExecute {
			return m, true
		}
	}
	if v.Decision == decisionExecute {
		return allow(LinkPolicy, "The standing policy would run it now."), true
	}
	sentence := v.Reason
	if sentence == "" {
		sentence = "Held back with no reason recorded."
	}
	return reasonMapping{link: LinkPolicy, sentence: sentence,
		noFix: "the gate gave no reason pg_sage can map to a fix"}, false
}

// failingVerdict is the first verdict that fails link, with its mapping.
func failingVerdict(in Inputs, link LinkName) (Verdict, reasonMapping, bool) {
	for _, v := range in.Verdicts {
		m, _ := verdictLink(v)
		if m.link == link && !m.allows {
			return v, m, true
		}
	}
	return Verdict{}, reasonMapping{}, false
}

func failedBy(in Inputs, name LinkName, v Verdict, m reasonMapping) Link {
	sentence := m.sentence
	if v.Detail != "" {
		sentence += " (" + v.Detail + ")"
	}
	return Link{Name: name, Status: Fail, Sentence: sentence, Reason: v.Reason,
		Fix: resolveFix(m.fix, in, v.Detail), NoFix: m.noFix}
}

// allowedBy is the first verdict whose mapping lets link pass.
func allowedBy(in Inputs, name LinkName) (Link, bool) {
	for _, v := range in.Verdicts {
		m, _ := verdictLink(v)
		if m.allows && (m.link == name || v.Decision == decisionExecute) {
			sentence := m.sentence
			if m.link != name {
				sentence = "The gate would run it, so this does not hold it back."
			}
			return Link{Name: name, Status: Pass, Sentence: sentence, Reason: v.Reason}, true
		}
	}
	return Link{}, false
}

func policyLink(in Inputs) Link {
	if in.PolicyErr != nil {
		return unknownLink(LinkPolicy, "The gate's explanation failed; the other links "+
			"still ran.", in.PolicyErr)
	}
	if v, m, ok := failingVerdict(in, LinkPolicy); ok {
		return failedBy(in, LinkPolicy, v, m)
	}
	if link, ok := allowedBy(in, LinkPolicy); ok {
		return link
	}
	if len(in.Verdicts) == 0 {
		return Link{Name: LinkPolicy, Status: NotApplicable,
			Sentence: "No candidate to put to the standing policy."}
	}
	return Link{Name: LinkPolicy, Status: Unknown,
		Sentence: "The gate held it back at another link before deciding the policy."}
}

// factFunc reads a link from the caller's own facts; false when it has
// none.
type factFunc func(Inputs) (Link, bool)

// gateLink is a link the gate's verdict and the caller's facts both
// answer: a verdict failing it wins, then the facts, then a verdict
// letting the change run. With neither it is unknown (or does not apply
// when there is nothing to explain).
func gateLink(in Inputs, name LinkName, fact factFunc) Link {
	if v, m, ok := failingVerdict(in, name); ok {
		return failedBy(in, name, v, m)
	}
	if fact != nil {
		if link, ok := fact(in); ok {
			link.Name = name
			return link
		}
	}
	if in.PolicyErr != nil {
		return unknownLink(name, "The gate's explanation failed, so this is unknown.",
			in.PolicyErr)
	}
	if link, ok := allowedBy(in, name); ok {
		return link
	}
	if len(in.Verdicts) == 0 {
		return Link{Name: name, Status: NotApplicable, Sentence: "No candidate to check."}
	}
	return Link{Name: name, Status: Unknown,
		Sentence: "The gate stopped at another link before checking this one."}
}

func windowFact(in Inputs) (Link, bool) {
	w := in.Window
	if w == nil {
		return Link{}, false
	}
	if w.Open {
		return Link{Status: Pass, Sentence: "The maintenance window is open."}, true
	}
	link := Link{Status: Fail, Sentence: "Outside the maintenance window.",
		NoFix: "the window never opens; widen it in the standing policy"}
	if !w.NextOpen.IsZero() {
		at := w.NextOpen.UTC().Format(time.RFC3339)
		link.Sentence = "Outside the maintenance window; it opens at " + at + "."
		link.Fix, link.NoFix = &Fix{Kind: FixWaitUntil, Value: at}, ""
	}
	return link, true
}

// budgetFact fails a budget at or past its cap: the next change would
// exceed it. A zero limit allows none.
func budgetFact(in Inputs) (Link, bool) {
	b := in.Budget
	if b == nil {
		return Link{}, false
	}
	used := fmt.Sprintf("%d of %d changes used in the window", b.Used, b.Limit)
	if b.Used < b.Limit {
		return Link{Status: Pass, Sentence: "Budget has room: " + used + "."}, true
	}
	link := Link{Status: Fail, Sentence: "The change budget is spent: " + used + "."}
	if b.FreesAt.After(in.Now) {
		link.Fix = &Fix{Kind: FixWaitUntil, Value: b.FreesAt.UTC().Format(time.RFC3339)}
	} else {
		link.Fix = uiLink(pageTrust, in.Database)
		link.Fix.Note = "Raise the budget in the standing policy."
	}
	return link, true
}

func privilegeFact(in Inputs) (Link, bool) {
	if in.PrivilegesErr != nil {
		return unknownLink(LinkPrivilege, "The role's grants could not be read.",
			in.PrivilegesErr), true
	}
	p := in.Privileges
	if p == nil {
		return Link{}, false
	}
	m := reasonMappings["replica_mutation"]
	switch {
	case p.Replica:
		return Link{Status: Fail, Sentence: m.sentence, NoFix: m.noFix}, true
	case !p.StatStatements:
		return Link{Status: Fail, Sentence: "pg_stat_statements is not installed in this " +
			"database, so pg_sage sees no query statistics.",
			Fix: extensionFix(in.Provider)}, true
	case !p.QueryTextVisible:
		return Link{Status: Fail, Sentence: "Role " + p.Role + " cannot see other " +
			"roles' query text, so query findings cannot name the statement.",
			Fix: statsGrantFix(p.Role, in.Provider)}, true
	}
	return Link{Status: Pass, Sentence: "Role " + p.Role +
		" holds the grants pg_sage reads with, on a writable primary."}, true
}

func trustFact(in Inputs) (Link, bool) {
	switch in.TrustLevel {
	case "":
		return Link{}, false
	case "advisory", "autonomous":
		return Link{Status: Pass, Sentence: "Trust level is " + in.TrustLevel + "."}, true
	}
	reason := "unknown_trust_level"
	if in.TrustLevel == "observation" {
		reason = "observe_only"
	}
	m := reasonMappings[reason]
	return Link{Status: Fail, Sentence: m.sentence, Reason: reason,
		Fix: resolveFix(m.fix, in, "")}, true
}

func stopFact(in Inputs) (Link, bool) {
	s := in.Stop
	if !s.Stopped && !s.Fleet {
		return Link{Status: Pass, Sentence: "Emergency stop is off."}, true
	}
	m := reasonMappings["emergency_stop"]
	sentence := m.sentence
	if s.Fleet {
		sentence = "The fleet-wide emergency stop is on."
	}
	if s.By != "" && !s.At.IsZero() {
		sentence += fmt.Sprintf(" %s stopped it at %s.", s.By, s.At.UTC().Format(time.RFC3339))
	}
	return Link{Status: Fail, Sentence: sentence, Reason: "emergency_stop",
		Fix: resolveFix(m.fix, in, "")}, true
}
