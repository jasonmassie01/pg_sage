package changereview

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxStatementRunes caps one statement's SQL in a comment.
const maxStatementRunes = 500

// renderTail is everything after the findings: statements, what was
// dropped or not determined, and the receipt.
func renderTail(r Review) string {
	var b strings.Builder
	if len(r.Statements) > 0 {
		b.WriteString("\n#### Statements\n\n")
		for _, s := range r.Statements {
			b.WriteString(statementLine(s))
		}
	}
	if len(r.Dropped) > 0 {
		fmt.Fprintf(&b, "\n%d model claims or findings were dropped (uncited, ungrounded "+
			"or outside the diff).\n", len(r.Dropped))
	}
	if len(r.NotDetermined) > 0 {
		b.WriteString("\n#### Not determined\n\n")
		for _, n := range r.NotDetermined {
			fmt.Fprintf(&b, "- %s: %s\n", escapeMarkdown(n.Subject), escapeMarkdown(n.Reason))
		}
	}
	b.WriteString(receiptSection(r))
	return b.String()
}

func statementLine(s Statement) string {
	line := fmt.Sprintf("- %d. %s", s.Index, escapeMarkdown(s.Kind))
	if s.File != "" {
		line += " at " + escapeMarkdown(s.File)
		if s.Line > 0 {
			line += ":" + strconv.Itoa(s.Line)
		}
	}
	line += " (" + strings.ReplaceAll(s.Origin, "_", " ")
	if s.Origin != OriginSQLInput {
		line += fmt.Sprintf(", confidence %.2f", s.Confidence)
	}
	line += ")"
	if s.MatchedQueryID != nil {
		line += fmt.Sprintf(", matches queryid %d", *s.MatchedQueryID)
	} else if s.Origin == OriginModelExtracted {
		line += ", new shape"
	}
	sql := clipRunes(strings.Join(strings.Fields(s.SQL), " "), maxStatementRunes)
	// A comment marker inside SQL must not look like pg_sage's own.
	sql = strings.ReplaceAll(sql, "<!--", "< !--")
	fence := strings.Repeat("`", longestRun(sql, '`')+1)
	pad := ""
	if strings.HasPrefix(sql, "`") || strings.HasSuffix(sql, "`") {
		pad = " "
	}
	return line + ": " + fence + pad + sql + pad + fence + "\n"
}

// receiptSection is the receipt plan of an undecided review or the
// decided receipt.
func receiptSection(r Review) string {
	var b strings.Builder
	b.WriteString("\n#### Receipt\n\n")
	if r.Receipt == nil || r.ReceiptVerdict == "" || r.ReceiptVerdict == ReceiptPending {
		fmt.Fprintf(&b, "After this lands, pg_sage checks %d catalog post-conditions and "+
			"compares %d watched statements before and after (Welch test). Landing: %s.\n",
			len(r.Expectations.PostConditions), len(r.Expectations.WatchQueryIDs),
			landingText(r))
		return b.String()
	}
	rc := r.Receipt
	fmt.Fprintf(&b, "Receipt: %s", r.ReceiptVerdict)
	if rc.Reason != "" {
		b.WriteString(" (" + escapeMarkdown(rc.Reason) + ")")
	}
	fmt.Fprintf(&b, ". Landed %s; window %d h", landingText(r), rc.WindowHours)
	if rc.Tolerance != "" {
		b.WriteString("; prediction tolerance " + escapeMarkdown(rc.Tolerance))
	}
	b.WriteString(".\n")
	for _, t := range rc.Targets {
		fmt.Fprintf(&b, "- queryid %d: %s, %.2f ms → %.2f ms (%+.1f%%), %d → %d calls\n",
			t.QueryID, escapeMarkdown(t.Verdict), t.BeforeMeanMs, t.AfterMeanMs, t.ChangePct,
			t.BeforeCalls, t.AfterCalls)
	}
	for _, q := range rc.BornQueryIDs {
		fmt.Fprintf(&b, "- new queryid %d since landing: %.2f ms over %d calls\n", q.QueryID,
			q.MeanMs, q.Calls)
	}
	if len(rc.ContradictedBy) > 0 {
		b.WriteString("Contradicted by: " + escapeMarkdown(strings.Join(rc.ContradictedBy,
			", ")) + ".\n")
	}
	return b.String()
}

func landingText(r Review) string {
	switch {
	case r.LandingState == LandingLanded && r.LandedAt != nil && r.LandedLower != nil:
		return fmt.Sprintf("between %s and %s (%s)", r.LandedLower.UTC().Format(time.RFC3339),
			r.LandedAt.UTC().Format(time.RFC3339), r.LandingSource)
	case r.LandingState == "":
		return "not started"
	}
	return strings.ReplaceAll(r.LandingState, "_", " ")
}

// escapeMarkdown is untrusted text as one line that cannot open HTML or
// break out of the comment: whitespace collapsed, angle brackets escaped.
func escapeMarkdown(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(s)
}
