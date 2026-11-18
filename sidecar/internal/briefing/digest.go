package briefing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/outcomes"
)

// The digest (ux-02b) is what the scheduled briefing sends: what pg_sage
// did, what was verified or reverted, what it saved, what needs a person
// and what it could not do and why, in that order. Every number is
// computed here from the outcome ledger (ux-02a), the inbox counts
// (ux-01a) and the open held-back decisions (policy-07); a model may only
// word the one-sentence headline (digest_llm.go).

// Headline sources.
const (
	HeadlineDeterministic = "deterministic"
	HeadlineModel         = "model"
)

// Section keys, in their fixed order.
const (
	SectionDid      = "did"
	SectionVerified = "verified"
	SectionSaved    = "saved"
	SectionNeedsYou = "needs_you"
	SectionCouldNot = "could_not"
)

// maxSectionItems caps the lines a section lists; its summary counts all.
const maxSectionItems = 10

// InboxCounts is ux-01a's count of what waits for a person on one
// database. LowerBound says a source could not be read.
type InboxCounts struct {
	Total      int            `json:"total"`
	ByKind     map[string]int `json:"by_kind"`
	LowerBound bool           `json:"lower_bound"`
}

// DigestCounts are the digest's numbers. SavedDollars is set only with an
// operator rate (briefing.dba_hour_rate_usd; decision D4).
type DigestCounts struct {
	Changes            int      `json:"changes"`
	Verified           int      `json:"verified"`
	Reverted           int      `json:"reverted"`
	Inconclusive       int      `json:"inconclusive"`
	Pending            int      `json:"pending"`
	SavedHours         float64  `json:"saved_hours"`
	SavedDollars       *float64 `json:"saved_dollars,omitempty"`
	NeedsYou           int      `json:"needs_you"`
	NeedsYouLowerBound bool     `json:"needs_you_lower_bound,omitempty"`
	HeldBack           int      `json:"held_back"`
}

// SectionItem is one line of a section.
type SectionItem struct {
	Label  string `json:"label"`
	Count  int    `json:"count,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Section is one part of the digest.
type Section struct {
	Key     string        `json:"key"`
	Title   string        `json:"title"`
	Summary string        `json:"summary"`
	Items   []SectionItem `json:"items"`
}

// Digest is one database's digest. Hash covers what it says, not when it
// was made or how the headline is worded, so an unchanged digest is not
// sent twice.
type Digest struct {
	Database         string       `json:"database,omitempty"`
	Window           Window       `json:"window"`
	Headline         string       `json:"headline"`
	HeadlineSource   string       `json:"headline_source"`
	HeadlineFallback string       `json:"headline_fallback,omitempty"`
	Counts           DigestCounts `json:"counts"`
	Sections         []Section    `json:"sections"`
	Gaps             []string     `json:"gaps"`
	Hash             string       `json:"hash"`
	// Names are the identifiers the headline must not repeat; never stored.
	names []string
}

// DigestInput is what a digest is built from; an error field means that
// source could not be read and becomes a gap.
type DigestInput struct {
	Database       string
	Window         Window
	Outcomes       outcomes.Result
	OutcomesErr    error
	Inbox          *InboxCounts
	InboxErr       error
	HeldBack       []ledger.WithheldEntry
	HeldBackErr    error
	DBAHourRateUSD float64
}

// BuildDigest builds the digest of in with its deterministic headline.
func BuildDigest(in DigestInput) Digest {
	d := Digest{Database: in.Database, Window: in.Window, Gaps: []string{}}
	d.Counts = countsOf(in)
	d.Sections = []Section{didSection(in), verifiedSection(d.Counts),
		savedSection(d.Counts), needsSection(in, d.Counts), couldNotSection(in, d.Counts)}
	for _, gap := range []struct {
		err  error
		name string
	}{{in.OutcomesErr, "the outcome ledger"}, {in.InboxErr, "the inbox"},
		{in.HeldBackErr, "the held-back decisions"}} {
		if gap.err != nil {
			d.Gaps = append(d.Gaps, gap.name+" could not be read")
		}
	}
	d.names = digestNames(in)
	d.Headline, d.HeadlineSource = DeterministicHeadline(d.Counts), HeadlineDeterministic
	d.Hash = digestHash(d)
	return d
}

func countsOf(in DigestInput) DigestCounts {
	s := in.Outcomes.Summary
	c := DigestCounts{Changes: s.Changes, Verified: s.Verified, Reverted: s.Reverted,
		Inconclusive: s.Inconclusive, Pending: s.Pending, HeldBack: len(in.HeldBack)}
	var saved float64
	for _, r := range in.Outcomes.Rows {
		if r.SavedHours != nil {
			saved += *r.SavedHours
		}
	}
	c.SavedHours = math.Round(saved*10) / 10
	if in.DBAHourRateUSD > 0 && !math.IsInf(in.DBAHourRateUSD, 0) {
		dollars := math.Round(c.SavedHours * in.DBAHourRateUSD)
		c.SavedDollars = &dollars
	}
	if in.Inbox != nil {
		c.NeedsYou, c.NeedsYouLowerBound = in.Inbox.Total, in.Inbox.LowerBound
	}
	return c
}

// Empty reports a digest with nothing in it: no change, nothing waiting,
// nothing held back and every source read.
func (d Digest) Empty() bool {
	c := d.Counts
	return c.Changes == 0 && c.NeedsYou == 0 && c.HeldBack == 0 && len(d.Gaps) == 0
}

// DeterministicHeadline is the headline without a model: "6 changes, 5
// verified, 1 reverted, 2 need you", or "Nothing changed".
func DeterministicHeadline(c DigestCounts) string {
	if c.Changes == 0 && c.NeedsYou == 0 && c.HeldBack == 0 {
		return "Nothing changed"
	}
	parts := []string{plural(c.Changes, "change", "changes")}
	if c.Changes > 0 {
		parts = append(parts, fmt.Sprintf("%d verified", c.Verified))
		if c.Reverted > 0 {
			parts = append(parts, fmt.Sprintf("%d reverted", c.Reverted))
		}
	}
	if c.NeedsYou > 0 {
		parts = append(parts, plural(c.NeedsYou, "needs you", "need you"))
	}
	if c.Changes == 0 && c.NeedsYou == 0 {
		parts = append(parts, fmt.Sprintf("%d held back", c.HeldBack))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// digestHash is the hex SHA-256 of what d says: its database, counts,
// sections and gaps; never its window or headline.
func digestHash(d Digest) string {
	body, _ := json.Marshal(struct { // plain values always marshal
		Database string       `json:"database"`
		Counts   DigestCounts `json:"counts"`
		Sections []Section    `json:"sections"`
		Gaps     []string     `json:"gaps"`
	}{d.Database, d.Counts, d.Sections, d.Gaps})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func didSection(in DigestInput) Section {
	s := Section{Key: SectionDid, Title: "Did",
		Summary: plural(in.Outcomes.Summary.Changes, "change", "changes"),
		Items:   []SectionItem{}}
	for _, r := range in.Outcomes.Rows {
		if len(s.Items) == maxSectionItems {
			break
		}
		detail := strings.ReplaceAll(r.Verdict, "_", " ")
		if r.RollbackState == outcomes.RollbackRolledBack {
			detail += ", rolled back"
		}
		s.Items = append(s.Items, SectionItem{Label: r.What, Detail: detail})
	}
	return s
}

func verifiedSection(c DigestCounts) Section {
	return Section{Key: SectionVerified, Title: "Verified or reverted",
		Summary: fmt.Sprintf("%d verified, %d reverted, %d inconclusive, %d pending",
			c.Verified, c.Reverted, c.Inconclusive, c.Pending), Items: []SectionItem{}}
}

func savedSection(c DigestCounts) Section {
	summary := fmt.Sprintf("%s DBA hours", formatHours(c.SavedHours))
	if c.SavedDollars != nil {
		summary += fmt.Sprintf(" ($%.0f)", *c.SavedDollars)
	}
	return Section{Key: SectionSaved, Title: "Saved", Summary: summary, Items: []SectionItem{}}
}

func formatHours(h float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", h), ".0")
}

func needsSection(in DigestInput, c DigestCounts) Section {
	summary := plural(c.NeedsYou, "item", "items")
	if c.NeedsYouLowerBound {
		summary = "at least " + summary
	}
	s := Section{Key: SectionNeedsYou, Title: "Needs you", Summary: summary,
		Items: []SectionItem{}}
	if in.Inbox == nil {
		return s
	}
	for _, k := range inbox.Kinds() {
		if n := in.Inbox.ByKind[string(k)]; n > 0 {
			s.Items = append(s.Items, SectionItem{Label: string(k), Count: n})
		}
	}
	return s
}

// couldNotSection groups the held-back decisions by reason, most first.
func couldNotSection(in DigestInput, c DigestCounts) Section {
	counts := map[string]int{}
	for _, e := range in.HeldBack {
		// The reason, not each decision's budget or wait detail, groups them.
		counts[executor.HumanDecisionReason(e.Reason, "")]++
	}
	items := make([]SectionItem, 0, len(counts))
	for label, n := range counts {
		items = append(items, SectionItem{Label: label, Count: n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Label < items[j].Label
	})
	if len(items) > maxSectionItems {
		items = items[:maxSectionItems]
	}
	return Section{Key: SectionCouldNot, Title: "Could not do and why",
		Summary: fmt.Sprintf("%d held back", c.HeldBack), Items: items}
}

// Text is the digest as a channel message: a title line, the headline,
// then each section with its lines, and any source that could not be read.
func (d Digest) Text() string {
	var b strings.Builder
	title := "pg_sage digest"
	if d.Database != "" {
		title += " · " + d.Database
	}
	fmt.Fprintf(&b, "%s (%s)\n%s\n", title, d.Window.label(), d.Headline)
	for _, s := range d.Sections {
		fmt.Fprintf(&b, "\n%s: %s\n", s.Title, s.Summary)
		for _, it := range s.Items {
			b.WriteString("- " + it.Label)
			if it.Count > 0 {
				fmt.Fprintf(&b, " (%d)", it.Count)
			}
			if it.Detail != "" {
				b.WriteString(": " + it.Detail)
			}
			b.WriteString("\n")
		}
	}
	for _, g := range d.Gaps {
		b.WriteString("\nNote: " + g + "\n")
	}
	return b.String()
}
