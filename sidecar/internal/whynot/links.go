package whynot

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// Collection freshness values the fleet reports.
const (
	freshFresh = "fresh"
	freshStale = "stale"
)

func observedLink(in Inputs) Link {
	o := in.Observed
	link := Link{Name: LinkObserved}
	switch {
	case o.LastSnapshot.IsZero() && o.Interval <= 0:
		link.Status, link.Sentence = Fail, "Not observed yet; the collector has no interval set."
		link.Fix = &Fix{Kind: FixSetting, Value: "collector.interval_seconds=60",
			Note: "Set a collection interval so pg_sage samples the database."}
	case o.LastSnapshot.IsZero():
		due := in.Now.Add(o.Interval).UTC().Format(time.RFC3339)
		link.Status = Fail
		link.Sentence = "Not observed yet; first snapshot due at " + due + "."
		link.Fix = &Fix{Kind: FixWaitUntil, Value: due}
	case o.Freshness == freshStale:
		link.Status = Fail
		link.Sentence = "Last snapshot " + age(in.Now, o.LastSnapshot) +
			" ago: the collector is stale."
		link.Fix = uiLink(pageDatabase, in.Database)
	case o.Freshness == freshFresh:
		link.Status = Pass
		link.Sentence = "Observed " + age(in.Now, o.LastSnapshot) + " ago."
	default:
		link.Status = Unknown
		link.Sentence = "Last snapshot " + age(in.Now, o.LastSnapshot) +
			" ago; its freshness cannot be judged yet."
	}
	return link
}

func age(now, at time.Time) string {
	d := now.Sub(at).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

func findingLink(in Inputs) Link {
	f := in.Findings
	link := Link{Name: LinkFinding, Status: Fail}
	switch {
	case f.Err != nil:
		return unknownLink(LinkFinding, "The open findings could not be read.", f.Err)
	case f.Open > 0:
		link.Status = Pass
		link.Sentence = fmt.Sprintf("%d open finding(s).", f.Open)
	case f.FindingID > 0:
		link.Sentence = fmt.Sprintf("Finding #%d is no longer open.", f.FindingID)
		link.NoFix = "it was resolved, suppressed or acted on"
	case !in.LLMEnabled && (f.Category == "" || llmDependent(f.Category)):
		link.Sentence = llmOffSentence(f.Category)
		link.Fix = &Fix{Kind: FixSetting, Value: "llm.enabled=true",
			Note: "Turn LLM-assisted analysis on in Settings; it needs llm.endpoint " +
				"and llm.api_key."}
	default:
		link.Sentence = "No open findings" + subjectOf(f.Category) +
			": pg_sage has seen nothing to change."
		link.NoFix = "there is nothing to fix"
	}
	return link
}

func llmOffSentence(category string) string {
	if category == "" {
		return "No open findings, and LLM is off: the advisor and the index optimizer " +
			"propose nothing."
	}
	return "LLM is off, so no " + category + " findings are produced."
}

func subjectOf(category string) string {
	if category == "" {
		return ""
	}
	return " in " + category
}

// llmDependent reports a category only an LLM-assisted producer emits:
// the advisor's and the index optimizer's, or one tuned by llm.* keys.
func llmDependent(category string) bool {
	c, ok := findingcat.Lookup(category)
	if !ok {
		return false
	}
	if c.Subsystem == "advisor" || c.Subsystem == "optimizer" {
		return true
	}
	for _, key := range c.ConfigKeys {
		if strings.HasPrefix(key, "llm.") {
			return true
		}
	}
	return false
}

func candidateLink(in Inputs) Link {
	link := Link{Name: LinkCandidate}
	switch {
	case in.CandidatesErr != nil:
		return unknownLink(LinkCandidate, "The action candidates could not be projected.",
			in.CandidatesErr)
	case in.Findings.Err != nil:
		return unknownLink(LinkCandidate, "No findings were read to project.", in.Findings.Err)
	case in.Findings.Open == 0:
		link.Status, link.Sentence = NotApplicable, "No open finding to act on."
	case in.Candidates > 0:
		link.Status = Pass
		link.Sentence = fmt.Sprintf("%d action candidate(s).", in.Candidates)
	default:
		link.Status = Fail
		link.Sentence = "The finding is advisory: pg_sage has no action it can take for it."
		link.NoFix = "advisory findings are for an operator to act on"
	}
	return link
}
