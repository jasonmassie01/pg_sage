package modelpass

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/review/report"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The final tools. Neither schema has a SQL field: the model's prose is
// stored as prose and never parsed for execution.
const (
	finalName  = "submit_review"
	refuteName = "submit_refutations"
	// maxItemClaims bounds the claims kept per entry.
	maxItemClaims = 5
)

const claimsSchema = `{"type":"array","items":{"type":"object","properties":{` +
	`"text":{"type":"string"},"evidence_ids":{"type":"array","items":{"type":"string"}}},` +
	`"required":["text","evidence_ids"]}}`

var reviewSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"ranking":{"type":"array","items":{"type":"object","properties":{` +
	`"candidate_id":{"type":"string"},"rank":{"type":"integer"},` +
	`"decision":{"type":"string","enum":["keep","drop","needs_evidence"]},` +
	`"attack":{"type":"string"},"claims":` + claimsSchema + `},` +
	`"required":["candidate_id","rank","decision","claims"]}},` +
	`"links":{"type":"array","items":{"type":"object","properties":{` +
	`"a":{"type":"string"},"b":{"type":"string"},"relation":{"type":"string",` +
	`"enum":["conflicts","depends_on","supersedes","related"]},"claims":` + claimsSchema +
	`},"required":["a","b","relation","claims"]}},` +
	`"hypotheses":{"type":"array","items":{"type":"object","properties":{` +
	`"title":{"type":"string"},"kind":{"type":"string"},"target":{"type":"string"},` +
	`"rationale":{"type":"string"},"claims":` + claimsSchema + `},` +
	`"required":["title","kind","target","claims"]}},` +
	`"report":{"type":"array","items":{"type":"object","properties":{` +
	`"dimension":{"type":"string"},"text":{"type":"string"},"claims":` + claimsSchema +
	`},"required":["dimension","text","claims"]}}},"required":["ranking"]}`)

var refuteSchema = json.RawMessage(`{"type":"object","properties":{"refutations":` +
	`{"type":"array","items":{"type":"object","properties":{"candidate_id":` +
	`{"type":"string"},"argument":{"type":"string"},"claims":` + claimsSchema + `},` +
	`"required":["candidate_id","argument","claims"]}}},"required":["refutations"]}`)

type rankItem struct {
	CandidateID string            `json:"candidate_id"`
	Rank        int               `json:"rank"`
	Decision    string            `json:"decision"`
	Attack      string            `json:"attack,omitempty"`
	Claims      []agentloop.Claim `json:"claims"`
}

type linkItem struct {
	A        string            `json:"a"`
	B        string            `json:"b"`
	Relation string            `json:"relation"`
	Claims   []agentloop.Claim `json:"claims"`
}

type hypothesis struct {
	Title     string            `json:"title"`
	Kind      string            `json:"kind"`
	Target    string            `json:"target"`
	Rationale string            `json:"rationale,omitempty"`
	Claims    []agentloop.Claim `json:"claims"`
}

type reportItem struct {
	Dimension string            `json:"dimension"`
	Text      string            `json:"text"`
	Claims    []agentloop.Claim `json:"claims"`
}

type submitted struct {
	Ranking    []rankItem   `json:"ranking"`
	Links      []linkItem   `json:"links"`
	Hypotheses []hypothesis `json:"hypotheses"`
	Report     []reportItem `json:"report"`
}

type refutation struct {
	CandidateID string            `json:"candidate_id"`
	Argument    string            `json:"argument"`
	Claims      []agentloop.Claim `json:"claims"`
}

type refutations struct {
	Refutations []refutation `json:"refutations"`
}

var relations = map[string]bool{"conflicts": true, "depends_on": true, "supersedes": true,
	"related": true}

var decisions = map[string]bool{report.DecisionKeep: true, report.DecisionDrop: true,
	report.DecisionNeedsEvidence: true}

// checker filters one run's answer against the run's evidence and the
// review's candidates, recording what it refuses.
type checker struct {
	attempt  int
	evidence map[string]agentloop.Evidence
	known    map[string]bool
	rejected []report.Rejected
	dropped  []agentloop.DroppedClaim
	kept     int
}

func (c *checker) reject(item, reason string) {
	c.rejected = append(c.rejected, report.Rejected{Attempt: c.attempt, Item: clip(item, 120),
		Reason: reason})
}

// claims keeps the claims that cite the run's evidence with grounded
// numbers.
func (c *checker) claims(in []agentloop.Claim) []agentloop.Claim {
	kept, dropped := agentloop.FilterClaims(in, c.evidence, sre.GroundClaim, maxItemClaims)
	c.dropped = append(c.dropped, dropped...)
	c.kept += len(kept)
	return kept
}

// prose is text when every number in it appears in the evidence its kept
// claims cite, else "" (recorded as a dropped claim).
func (c *checker) prose(text string, kept []agentloop.Claim) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var cited []agentloop.Evidence
	for _, k := range kept {
		for _, a := range k.Aliases {
			cited = append(cited, c.evidence[a])
		}
	}
	if sre.GroundClaim(text, cited) != nil {
		c.dropped = append(c.dropped, agentloop.DroppedClaim{Text: clip(text, 200),
			Reason: agentloop.DropUngrounded})
		return ""
	}
	return text
}

// decodeReview reads a submit_review answer; extra fields are ignored.
func decodeReview(raw json.RawMessage) (submitted, error) {
	var s submitted
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("the %s answer does not decode: %v", finalName, err)
	}
	return s, nil
}

// rankings keeps the valid ranking entries, one per known candidate.
func (c *checker) rankings(items []rankItem) []rankItem {
	var out []rankItem
	seen := map[string]bool{}
	for _, it := range items {
		switch {
		case !c.known[it.CandidateID]:
			c.reject(it.CandidateID, "unknown candidate")
		case seen[it.CandidateID]:
			c.reject(it.CandidateID, "duplicate candidate")
		case !decisions[it.Decision]:
			c.reject(it.CandidateID, "invalid decision")
		case it.Rank < 1:
			c.reject(it.CandidateID, "invalid rank")
		default:
			seen[it.CandidateID] = true
			it.Claims = c.claims(it.Claims)
			it.Attack = c.prose(it.Attack, it.Claims)
			out = append(out, it)
		}
	}
	return out
}

func (c *checker) links(items []linkItem) []linkItem {
	var out []linkItem
	for _, it := range items {
		switch {
		case !c.known[it.A] || !c.known[it.B] || it.A == it.B:
			c.reject(it.A+" "+it.B, "unknown candidate")
			continue
		case !relations[it.Relation]:
			c.reject(it.Relation, "invalid relation")
			continue
		}
		if it.Claims = c.claims(it.Claims); len(it.Claims) == 0 {
			c.reject(it.A+" "+it.B, "uncited link")
			continue
		}
		out = append(out, it)
	}
	return out
}

func (c *checker) hypotheses(items []hypothesis) []hypothesis {
	var out []hypothesis
	for _, it := range items {
		if strings.TrimSpace(it.Title) == "" || strings.TrimSpace(it.Kind) == "" ||
			strings.TrimSpace(it.Target) == "" {
			c.reject(it.Title, "incomplete hypothesis")
			continue
		}
		if it.Claims = c.claims(it.Claims); len(it.Claims) == 0 {
			c.reject(it.Title, "uncited hypothesis")
			continue
		}
		it.Rationale = c.prose(it.Rationale, it.Claims)
		out = append(out, it)
	}
	return out
}

func (c *checker) reports(items []reportItem) []reportItem {
	var out []reportItem
	for _, it := range items {
		if it.Claims = c.claims(it.Claims); len(it.Claims) == 0 {
			c.reject(it.Dimension, "uncited report text")
			continue
		}
		if it.Text = c.prose(it.Text, it.Claims); it.Text == "" {
			c.reject(it.Dimension, "ungrounded report text")
			continue
		}
		out = append(out, it)
	}
	return out
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
