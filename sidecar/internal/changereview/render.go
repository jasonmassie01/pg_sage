package changereview

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SuggestionLabel precedes every model suggestion in a comment.
const SuggestionLabel = "suggested by pg_sage's model — verify"

// RenderOptions configure the comment: UIBaseURL links the review in the
// pg_sage UI (relative "#/reviews?..." when empty); MaxRunes caps the
// comment (MaxCommentRunes when 0).
type RenderOptions struct {
	UIBaseURL string
	MaxRunes  int
}

var markerUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.\-]`)

// Marker is line 1 of every comment, which the CLI's GitHub mode finds to
// update its own comment: <!-- pg_sage-review db=<name> id=<uuid>
// receipt=<hash|pending> -->.
func Marker(r Review) string {
	receipt := r.ReceiptHash
	if receipt == "" {
		receipt = "pending"
	}
	return "<!-- pg_sage-review db=" + markerUnsafe.ReplaceAllString(r.Database, "_") +
		" id=" + markerUnsafe.ReplaceAllString(r.ID, "_") + " receipt=" +
		markerUnsafe.ReplaceAllString(receipt, "_") + " -->"
}

// ReviewLink is the review's page in the pg_sage UI, or "" without the
// UI's address (change_review.ui_base_url): a relative link would resolve
// against GitHub or a chat client and open nothing.
func ReviewLink(r Review, o RenderOptions) string {
	base := strings.TrimRight(o.UIBaseURL, "/")
	if base == "" {
		return ""
	}
	return base + "/#/reviews?id=" + r.ID + "&database=" + markerUnsafe.ReplaceAllString(
		r.Database, "_")
}

// reviewPointer is where the full review is: its UI link, or its id on
// the Reviews page.
func reviewPointer(r Review, o RenderOptions) string {
	if link := ReviewLink(r, o); link != "" {
		return link
	}
	return "review " + markerUnsafe.ReplaceAllString(r.ID, "_") + " on pg_sage's Reviews page"
}

// RenderMarkdown is the review comment: marker, verdict, calibration,
// findings by severity (each line ending with its evidence ids), model
// suggestions fenced and labeled, statements, what was dropped or not
// determined, and the receipt plan or decided receipt. It never exceeds
// the rune cap: findings that do not fit are summarized in a note that
// links the UI.
func RenderMarkdown(r Review, o RenderOptions) string {
	limit := o.MaxRunes
	if limit <= 0 || limit > MaxCommentRunes {
		limit = MaxCommentRunes
	}
	head := renderHead(r)
	if r.State != StateComplete {
		return clipRunes(head, limit)
	}
	tail := renderTail(r)
	var b strings.Builder
	b.WriteString(head)
	shown := 0
	blocks := findingBlocks(r.Findings)
	note := func(left int) string {
		return fmt.Sprintf("\n_%d more findings not shown; see the full review: %s_\n",
			left, reviewPointer(r, o))
	}
	used := utf8.RuneCountInString(head)
	noteRunes := utf8.RuneCountInString(note(len(blocks)))
	if used+noteRunes+utf8.RuneCountInString(tail) > limit {
		tail = ""
	}
	reserve := utf8.RuneCountInString(tail) + noteRunes
	for _, blk := range blocks {
		n := utf8.RuneCountInString(blk)
		if used+n+reserve > limit {
			break
		}
		b.WriteString(blk)
		used += n
		shown++
	}
	if shown < len(blocks) {
		b.WriteString(note(len(blocks) - shown))
	}
	b.WriteString(tail)
	return clipRunes(b.String(), limit)
}

// renderHead is the marker, verdict line and model status.
func renderHead(r Review) string {
	var b strings.Builder
	b.WriteString(Marker(r) + "\n")
	switch r.State {
	case StateRunning:
		b.WriteString("### pg_sage database review: running\n\nThe review of " +
			escapeMarkdown(r.Database) + " is still running.\n")
		return b.String()
	case StateFailed:
		b.WriteString("### pg_sage database review: failed\n\n" +
			escapeMarkdown(r.FailureReason) + "\n")
		return b.String()
	}
	fmt.Fprintf(&b, "### pg_sage database review: %s\n\n", strings.ToUpper(r.Verdict))
	fmt.Fprintf(&b, "Database %s, deterministic verdict %s", escapeMarkdown(r.Database),
		r.DeterministicVerdict)
	if r.Verdict != r.DeterministicVerdict {
		fmt.Fprintf(&b, ", raised to %s by model findings", r.Verdict)
	}
	b.WriteString(". pg_sage read the catalog and statistics and planned statements " +
		"(plan-only); it ran nothing.\n")
	if r.Summary.Calibration != nil {
		b.WriteString("\n" + r.Summary.Calibration.Line() + "\n")
	}
	b.WriteString("\n" + modelLine(r) + "\n")
	b.WriteString(dbaLine(r))
	return b.String()
}

// dbaLine is the DBA's verdict as the developer must act on it: the
// verdict, an override's direction, who, and the edits or reason given.
func dbaLine(r Review) string {
	if r.DBAVerdict == "" {
		return ""
	}
	verdict := strings.ReplaceAll(r.DBAVerdict, "_", " ")
	if r.DBAVerdict == DBAOverride && r.DBADirection != "" {
		verdict += " (pg_sage was " + strings.ReplaceAll(r.DBADirection, "_", " ") + ")"
	}
	line := fmt.Sprintf("\nDBA verdict: %s by %s", verdict, escapeMarkdown(r.DBAActor))
	if r.VerdictCarriedFrom != "" {
		line += fmt.Sprintf(" (carried over from review %s)", r.VerdictCarriedFrom)
	}
	line += "."
	if reason := escapeMarkdown(r.DBAReason); reason != "" {
		label := "Reason"
		if r.DBAVerdict == DBAConcurWithEdits {
			label = "Edits"
		}
		line += " " + label + ": " + reason
	}
	return line + "\n"
}

func modelLine(r Review) string {
	if r.ModelStatus == ModelOK {
		return fmt.Sprintf("Model: ok, %d tokens; %d dropped for missing or ungrounded "+
			"citations.", r.Tokens, len(r.Dropped))
	}
	return fmt.Sprintf("Model: %s. App-code changes were not analyzed; the verdict is "+
		"deterministic.", r.ModelStatus)
}

// findingBlocks renders each finding (with its suggestion) grouped by
// severity, critical first; the first block of a group carries its
// heading.
func findingBlocks(fs []Finding) []string {
	var out []string
	for _, sev := range []string{SeverityCritical, SeverityWarning, SeverityInfo} {
		first := true
		for _, f := range fs {
			if f.Severity != sev {
				continue
			}
			blk := findingBlock(f)
			if first {
				blk = "\n#### " + strings.ToUpper(sev[:1]) + sev[1:] + "\n\n" + blk
				first = false
			}
			out = append(out, blk)
		}
	}
	return out
}

func findingBlock(f Finding) string {
	id := f.RuleID
	if id == "" {
		id = "model"
	}
	line := fmt.Sprintf("- **%s** %s: %s", id, escapeMarkdown(f.Kind),
		escapeMarkdown(f.Title))
	if t := escapeMarkdown(f.Text); t != "" {
		line += " — " + t
	}
	ev := "none"
	if len(f.EvidenceIDs) > 0 {
		ev = strings.Join(f.EvidenceIDs, ", ")
	}
	out := line + " (evidence: " + escapeMarkdown(ev) + ")\n"
	if f.Suggestion != nil && f.Suggestion.Code != "" {
		out += suggestionBlock(*f.Suggestion)
	}
	return out
}

// suggestionBlock fences a model suggestion with a fence longer than any
// backtick run in it, so the code cannot close the fence early.
func suggestionBlock(s Suggestion) string {
	code := strings.ReplaceAll(clipRunes(s.Code, maxSuggestionCode), "<!--", "< !--")
	fence := strings.Repeat("`", longestRun(code, '`')+1)
	if len(fence) < 3 {
		fence = "```"
	}
	lang := markerUnsafe.ReplaceAllString(s.Language, "")
	return "\n  _" + SuggestionLabel + "_\n\n" + fence + lang + "\n" + code + "\n" + fence +
		"\n\n"
}

func longestRun(s string, c rune) int {
	best, cur := 0, 0
	for _, r := range s {
		if r == c {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return best
}

// RenderJSON is the machine-readable comment: the review without its
// transcript, plus the marker and the Markdown.
func RenderJSON(r Review) ([]byte, error) {
	r.Transcript = nil
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	marker, _ := json.Marshal(Marker(r))
	obj["marker"] = marker
	return json.Marshal(obj)
}
