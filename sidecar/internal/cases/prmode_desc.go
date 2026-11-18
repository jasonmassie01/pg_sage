package cases

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/verify"
)

// prSection is one "## title" section of a PR description.
type prSection struct{ title, body string }

// description is the PR body, built in code: the plain-words paragraph,
// why (with the evidence ids), the expected effect from the ladder
// receipt, risk and rollback, the call site, then the hidden marker.
func (m *PRMode) description(ctx context.Context, c PRCandidate, ch prChange,
	key string) string {
	sections := []prSection{
		{"What this changes", m.paragraph(ctx, c, ch)},
		{"Why", whySection(c)},
		{"Expected effect", effectSection(c.Ladder)},
		{"Risk and rollback", riskSection(c)},
		{"Call site", callSiteSection(c, ch)},
	}
	return renderPRBody(sections, prMarker(key), MaxPRBody)
}

func whySection(c PRCandidate) string {
	why := strings.TrimSpace(c.Why)
	if why == "" {
		why = strings.TrimSpace(c.Title)
	}
	ids := append([]string{fmt.Sprintf("finding:%d", c.FindingID)}, c.EvidenceIDs...)
	return why + "\n\nEvidence: " + strings.Join(uniqueStrings(ids), ", ") + "."
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// effectSection is the ladder receipt (lab-03a): the highest tier passed
// first, then every record.
func effectSection(ladder []verify.LadderRecord) string {
	best, ok := verify.HighestPassed(ladder)
	if !ok {
		return "No verification tier has passed yet. pg_sage measures the effect " +
			"after the deploy is reported and records the verdict on the finding."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Highest tier passed: %s.\n", ladderLine(best))
	for _, r := range ladder {
		fmt.Fprintf(&b, "\n- %s: %s", ladderLine(r), r.Result)
	}
	b.WriteString("\n\npg_sage measures the effect after the deploy and records the verdict.")
	return b.String()
}

func ladderLine(r verify.LadderRecord) string {
	line := fmt.Sprintf("Tier %d (%s)", r.Tier, r.Name)
	if r.Predicted != nil {
		line += fmt.Sprintf(", predicted %+.1f%% %s", *r.Predicted, r.Metric)
	}
	if r.Method != "" {
		line += " by " + r.Method
	}
	return line
}

func riskSection(c PRCandidate) string {
	tier := c.RiskTier
	if tier == "" {
		tier = "high"
	}
	b := "Risk tier: " + tier + ". A person reviews and merges; pg_sage does not run it."
	switch {
	case c.Kind == PRKindRewrite:
		return b + "\n\nRollback (application_rollback): revert this PR's commit."
	case strings.TrimSpace(c.RollbackSQL) == "":
		return b + "\n\nRollback: none. This migration is a forward fix only."
	}
	return b + "\n\nRollback:\n\n```sql\n" + strings.TrimSpace(c.RollbackSQL) + "\n```"
}

func callSiteSection(c PRCandidate, ch prChange) string {
	if c.Kind == PRKindRewrite {
		return "Called from " + ch.callSite + "."
	}
	path := ""
	if len(ch.files) > 0 {
		path = ch.files[0].Path
	}
	return "Not applicable: a new migration file, " + path + "."
}

// renderPRBody joins the sections and the marker. Over limit characters,
// whole sections are cut from the end and named; the marker always stays.
func renderPRBody(sections []prSection, marker string, limit int) string {
	var cut []string
	for n := len(sections); n >= 1; n-- {
		body := joinSections(sections[:n], cut, marker)
		if len([]rune(body)) <= limit {
			return body
		}
		cut = append([]string{sections[n-1].title}, cut...)
	}
	// Even the first section is too long: keep its head.
	note := cutNote(cut)
	room := limit - len([]rune(marker+note)) - 4
	head := []rune("## " + sections[0].title + "\n\n" + sections[0].body)
	if room < 0 {
		room = 0
	}
	if len(head) > room {
		head = head[:room]
	}
	return string(head) + "\n\n" + note + marker + "\n"
}

func joinSections(sections []prSection, cut []string, marker string) string {
	var b strings.Builder
	for _, s := range sections {
		b.WriteString("## " + s.title + "\n\n" + strings.TrimSpace(s.body) + "\n\n")
	}
	b.WriteString(cutNote(cut))
	b.WriteString(marker + "\n")
	return b.String()
}

func cutNote(cut []string) string {
	if len(cut) == 0 {
		return ""
	}
	return "Cut to fit the host's limit: " + strings.Join(cut, ", ") +
		". The case in pg_sage has them.\n\n"
}
