package ask

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// compose turns a finished loop into the answer a person sees. Only a run
// that concluded keeps claims; a claim survives only when every evidence
// id it cites is evidence this run read (the loop's citation filter
// already dropped uncited, unknown and ungrounded claims). The model's
// "not observed" notes are kept when short and free of numbers (a number
// is a claim and needs a citation).
func compose(database string, res agentloop.Result, cited map[string]agentloop.Evidence,
	actions []ActionTaken) Answer {
	a := Answer{Database: database, Stop: res.Transcript.Stop, Tokens: res.Transcript.Tokens,
		Statements: []Statement{}, NotVerified: []string{},
		Dropped: append([]agentloop.DroppedClaim{}, res.Dropped...), Citations: []Citation{},
		Actions: append([]ActionTaken{}, actions...)}
	if a.Stop == agentloop.StopFinal {
		keepClaims(&a, res.Claims, cited)
		keepNotes(&a, res.Final)
	}
	a.Status = statusOf(a)
	a.Text = render(a)
	return a
}

func keepClaims(a *Answer, claims []agentloop.Claim, cited map[string]agentloop.Evidence) {
	seen := map[string]bool{}
	for _, c := range claims {
		if !allKnown(c.EvidenceIDs, cited) {
			a.Dropped = append(a.Dropped, agentloop.DroppedClaim{Text: clip(c.Text, 200),
				Reason: agentloop.DropUnknownEvidence})
			continue
		}
		a.Statements = append(a.Statements, Statement{Text: c.Text,
			Citations: append([]string{}, c.EvidenceIDs...)})
		for _, id := range c.EvidenceIDs {
			if !seen[id] {
				seen[id] = true
				a.Citations = append(a.Citations, citationFor(a.Database, cited[id]))
			}
		}
	}
}

func allKnown(ids []string, cited map[string]agentloop.Evidence) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if _, ok := cited[id]; !ok {
			return false
		}
	}
	return true
}

func keepNotes(a *Answer, final json.RawMessage) {
	var body struct {
		NotObserved []json.RawMessage `json:"not_observed"`
	}
	if json.Unmarshal(final, &body) != nil {
		return
	}
	seen := map[string]bool{}
	for _, raw := range body.NotObserved {
		var note string
		if json.Unmarshal(raw, &note) != nil {
			continue
		}
		note = strings.Join(strings.Fields(note), " ")
		reason := ""
		switch {
		case note == "" || seen[strings.ToLower(note)]:
			continue
		case utf8.RuneCountInString(note) > MaxNoteRunes:
			reason = DropNoteTooLong
		case strings.IndexFunc(note, unicode.IsDigit) >= 0:
			reason = DropNoteNumbers
		case len(a.NotVerified) >= MaxNotes:
			continue
		}
		if reason != "" {
			a.Dropped = append(a.Dropped, agentloop.DroppedClaim{Text: clip(note, 200),
				Reason: reason})
			continue
		}
		seen[strings.ToLower(note)] = true
		a.NotVerified = append(a.NotVerified, note)
	}
}

func statusOf(a Answer) string {
	switch a.Stop {
	case agentloop.StopFinal:
		if len(a.Statements) > 0 {
			return StatusAnswered
		}
		return StatusNotObserved
	case agentloop.StopBudget:
		return StatusBudget
	case agentloop.StopDisabled:
		return StatusNoModel
	}
	return StatusIncomplete
}

// citationFor describes one evidence item; the API path is where the
// cited object is served (none for config, docs, tables and queries).
func citationFor(database string, ev agentloop.Evidence) Citation {
	kind, ref, _ := strings.Cut(ev.ID, ":")
	c := Citation{ID: ev.ID, Kind: kind, Ref: ref, Label: ev.Label, Digest: ev.Digest}
	db := url.QueryEscape(database)
	switch kind {
	case "finding":
		c.APIPath = "/api/v1/findings/" + url.PathEscape(ref) + "?database=" + db
	case "findings":
		status, _, _ := strings.Cut(ref, ":")
		c.APIPath = "/api/v1/findings?database=" + db + "&status=" + url.QueryEscape(status)
	case "action":
		c.APIPath = "/api/v1/actions/" + url.PathEscape(ref) + "?database=" + db
	case "actions":
		c.APIPath = "/api/v1/actions?database=" + db
	case "approvals", "proposal":
		c.APIPath = "/api/v1/actions/pending?database=" + db
	case "trust":
		c.APIPath = "/api/v1/trust?database=" + db
	case "facts":
		c.APIPath = "/api/v1/facts?database=" + db
		if ref != "all" {
			c.APIPath += "&status=" + url.QueryEscape(ref)
		}
	case "incidents":
		c.APIPath = "/api/v1/incidents?database=" + db
	case "investigation":
		c.APIPath = "/api/v1/databases/" + url.PathEscape(database) + "/investigations/" +
			url.PathEscape(ref)
	case "investigations":
		c.APIPath = "/api/v1/databases/" + url.PathEscape(database) + "/investigations"
	}
	return c
}

// render is the answer as plain text: verified statements with their
// citation numbers, what could not be verified, what was dropped and the
// actions taken.
func render(a Answer) string {
	var b strings.Builder
	b.WriteString(lead(a))
	number := map[string]int{}
	for i, c := range a.Citations {
		number[c.ID] = i + 1
	}
	for _, s := range a.Statements {
		b.WriteString("\n" + s.Text)
		for _, id := range s.Citations {
			fmt.Fprintf(&b, " [%d]", number[id])
		}
	}
	if len(a.NotVerified) > 0 {
		b.WriteString("\nCould not verify:")
		for _, n := range a.NotVerified {
			b.WriteString("\n- " + n)
		}
	}
	if n := len(a.Dropped); n == 1 {
		b.WriteString("\n1 statement was dropped because it was not backed by evidence " +
			"I read.")
	} else if n > 1 {
		fmt.Fprintf(&b, "\n%d statements were dropped because they were not backed by "+
			"evidence I read.", n)
	}
	for _, act := range a.Actions {
		b.WriteString("\n" + actionLine(act))
	}
	return strings.TrimSpace(b.String())
}

func lead(a Answer) string {
	switch a.Status {
	case StatusNotObserved:
		return "I could not verify an answer from the evidence I could read."
	case StatusBudget:
		return "Ask Sage's daily LLM budget is used up (ask.daily_tokens_per_user / " +
			"ask.daily_tokens_per_database); it resets at 00:00 UTC."
	case StatusNoModel:
		return "Ask Sage needs an LLM: configure llm.* to ask questions."
	case StatusIncomplete:
		if len(a.Statements) == 0 {
			return "The answer was cut short (" + a.Stop + "); nothing could be verified."
		}
		return "The answer was cut short (" + a.Stop + ")."
	}
	return ""
}

func actionLine(a ActionTaken) string {
	what := a.Kind + " " + a.ID
	switch a.Status {
	case ActionQueued:
		return "Queued " + what + " for a person's approval (Actions page)."
	case ActionPending:
		return "The fix is already waiting for approval as " + what + "."
	case ActionOpened:
		return "Opened " + what + "."
	case ActionJoined:
		return "Joined the running " + what + "."
	}
	return "The " + a.Kind + " was not made (" + a.Status + "): " + a.Reason
}

// digestOf is the hex SHA-256 of what the model was shown.
func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// clip keeps at most n runes of s.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
