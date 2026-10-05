package approvalcard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// MaxTextRunes bounds a card's chat text: Slack's section block limit
// (Telegram allows 4096).
const MaxTextRunes = 3000

// ContentHash is the hash a decision on a card is bound to: the queue
// item, its finding, action type, risk and the exact SQL and rollback SQL
// (and the recommendation revision hash). A change to any of them after
// the card was sent makes the card's approval refuse.
func ContentHash(a store.QueuedAction) string {
	sum := sha256.New()
	for _, part := range []string{"approval-card-v1", strconv.Itoa(a.ID),
		strconv.Itoa(a.FindingID), a.ActionType, a.ActionRisk, a.ProposedSQL,
		a.RollbackSQL, a.ContentHash} {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Summary is the predicted effect in a few words ("" when none).
func Summary(c Card) string {
	p := c.Predicted
	var parts []string
	if p.ImprovementPct != nil {
		s := fmt.Sprintf("%.1f%% faster", *p.ImprovementPct)
		if n := len(p.AffectedQueries); n > 0 {
			s += fmt.Sprintf(" on %d quer%s", n, plural(n, "y", "ies"))
		}
		if p.Method == "hypopg" {
			s += " (HypoPG verified)"
		} else {
			s += " (estimate)"
		}
		parts = append(parts, s)
	}
	if p.EstimatedSizeBytes != nil {
		parts = append(parts, "about "+formatBytes(*p.EstimatedSizeBytes))
	}
	return strings.Join(parts, "; ")
}

// rationaleLabel names the rationale's source and its confidence: the
// tuning agent's calibration when it has one, else a producer's own
// confidence; an uncalibrated proposal shows no number.
func rationaleLabel(r *Rationale) string {
	label := "Rationale"
	if r.Source == "llm" {
		label = "Model rationale"
	}
	switch {
	case r.Calibration != "" && r.Confidence != nil:
		return label + fmt.Sprintf(" (calibrated confidence %.0f%%: %s)", *r.Confidence*100,
			r.Calibration)
	case r.Calibration != "":
		return label + " (" + r.Calibration + ")"
	case r.Confidence != nil:
		return label + fmt.Sprintf(" (confidence %.0f%%)", *r.Confidence*100)
	}
	return label
}

// Text renders a card as plain chat text (no markup), within MaxTextRunes.
func Text(c Card, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nDatabase: %s | Risk: %s | %s | queue item %d\n", c.Title,
		c.Database, c.Risk.Tier, expiresIn(c.ExpiresAt, now), c.QueueID)
	if o := c.Origin; o != nil {
		fmt.Fprintf(&b, "Proposed via %s by %s\n", o.Label, o.By)
	}
	b.WriteString("\nWhy it needs you:\n")
	for _, r := range c.Why {
		b.WriteString("- " + r.Text + "\n")
	}
	if w := c.VerificationWait; w != nil && w.Unavailable != "" {
		fmt.Fprintf(&b, "\n%s\n", w.Line)
	}
	if c.Trust != nil {
		fmt.Fprintf(&b, "\n%s\n", c.Trust.Line)
	}
	if len(c.Evidence) > 0 {
		b.WriteString("\nEvidence:\n")
		for _, e := range c.Evidence {
			fmt.Fprintf(&b, "- %s: %s [%s]\n", e.Label, truncate(e.Value, 200), e.Ref)
		}
	}
	if r := c.Rationale; r != nil {
		fmt.Fprintf(&b, "\n%s: %s\n", rationaleLabel(r), truncate(r.Text, 600))
	}
	if s := Summary(c); s != "" {
		fmt.Fprintf(&b, "\nPredicted effect: %s\n", s)
	}
	fmt.Fprintf(&b, "\nSQL:\n%s\n", truncate(c.SQL, 800))
	writeRollback(&b, c.Rollback)
	fmt.Fprintf(&b, "\nBlast radius: %s\n", c.Risk.BlastRadius)
	if c.Risk.Lock != "" {
		fmt.Fprintf(&b, "Lock: %s\n", c.Risk.Lock)
	}
	if len(c.Risk.Guardrails) > 0 {
		fmt.Fprintf(&b, "Guardrails: %s\n", strings.Join(c.Risk.Guardrails, ", "))
	}
	if c.SnoozedUntil != nil {
		fmt.Fprintf(&b, "Snoozed until %s: %s\n",
			c.SnoozedUntil.UTC().Format("2006-01-02 15:04 UTC"), c.SnoozeReason)
	}
	return truncate(strings.TrimRight(b.String(), "\n"), MaxTextRunes)
}

func writeRollback(b *strings.Builder, r Rollback) {
	label := "Rollback"
	if r.Class != "" {
		label += " (" + humanize(r.Class) + ")"
	}
	body := r.SQL
	if strings.TrimSpace(body) == "" {
		body = r.Note
	}
	fmt.Fprintf(b, "\n%s:\n%s\n", label, truncate(body, 600))
}

// expiresIn renders the time left on a card.
func expiresIn(at, now time.Time) string {
	if at.IsZero() {
		return "no expiry"
	}
	d := at.Sub(now)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return fmt.Sprintf("Expires in %dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("Expires in %dh", int(d.Hours()))
	}
}
