package briefing

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/outcomes"
)

// ux-02b: the digest is built in Go, in a fixed order, from the outcome
// ledger, the inbox counts and the held-back decisions.
// No concurrent access tests here: BuildDigest is a pure function of its
// input; the concurrent send is covered by digest_db_test.go.

var digestWindow = Window{Start: time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC),
	End: time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)}

func hours(h float64) *float64 { return &h }

// sixChanges is 6 changes: 5 verified (improved), 1 rolled back.
func sixChanges() outcomes.Result {
	var rows []outcomes.Row
	for i := range 5 {
		rows = append(rows, outcomes.Row{ActionID: int64(i + 1), What: "Index created",
			Verdict: outcomes.VerdictImproved, RollbackState: outcomes.RollbackAvailable,
			SavedHours: hours(0.5)})
	}
	rows = append(rows, outcomes.Row{ActionID: 6, What: "Setting changed",
		Verdict: outcomes.VerdictPending, RollbackState: outcomes.RollbackRolledBack,
		SavedHours: hours(0.25)})
	return outcomes.Result{Rows: rows, Summary: outcomes.Summarize(rows)}
}

func twoInboxItems() *InboxCounts {
	return &InboxCounts{Total: 2, ByKind: map[string]int{"change_approval": 1, "fact": 1}}
}

func heldBack() []ledger.WithheldEntry {
	return []ledger.WithheldEntry{
		{DecisionID: 1, Reason: "outside_maintenance_window", TargetObjects: []string{
			"public.orders"}},
		{DecisionID: 2, Reason: "outside_maintenance_window"},
		{DecisionID: 3, Reason: "trust_level_insufficient"},
	}
}

func happyInput() DigestInput {
	return DigestInput{Database: "prod", Window: digestWindow, Outcomes: sixChanges(),
		Inbox: twoInboxItems(), HeldBack: heldBack()}
}

func TestBuildDigest_FiveSectionsInOrderWithTheNumbers(t *testing.T) {
	d := BuildDigest(happyInput())
	var keys []string
	for _, s := range d.Sections {
		keys = append(keys, s.Key)
	}
	if got := strings.Join(keys, ","); got != "did,verified,saved,needs_you,could_not" {
		t.Fatalf("section order = %s", got)
	}
	c := d.Counts
	if c.Changes != 6 || c.Verified != 5 || c.Reverted != 1 || c.Pending != 1 ||
		c.NeedsYou != 2 || c.HeldBack != 3 {
		t.Fatalf("counts = %+v, want 6 changes, 5 verified, 1 reverted, 2 need you, 3 held",
			c)
	}
	if c.SavedHours != 2.8 {
		t.Fatalf("saved hours = %v, want 2.8 (5 x 0.5 + 0.25, one decimal)", c.SavedHours)
	}
	if c.SavedDollars != nil {
		t.Fatalf("saved dollars = %v, want none without an operator rate", *c.SavedDollars)
	}
	if d.Headline != "6 changes, 5 verified, 1 reverted, 2 need you" ||
		d.HeadlineSource != HeadlineDeterministic {
		t.Fatalf("headline = %q (%s)", d.Headline, d.HeadlineSource)
	}
	could := d.Sections[4]
	if len(could.Items) != 2 || could.Items[0].Count != 2 ||
		!strings.Contains(could.Items[0].Label, "maintenance window") {
		t.Fatalf("could-not items = %+v, want the window reason first with 2", could.Items)
	}
	needs := d.Sections[3]
	if len(needs.Items) != 2 || needs.Items[0].Label != "change_approval" {
		t.Fatalf("needs-you items = %+v, want change_approval then fact", needs.Items)
	}
	if len(d.Hash) != 64 {
		t.Fatalf("hash = %q, want a hex SHA-256", d.Hash)
	}
}

// Boundary: saved dollars appear only with an operator rate above zero.
func TestBuildDigest_DollarsOnlyWithARate(t *testing.T) {
	in := happyInput()
	in.DBAHourRateUSD = 150
	d := BuildDigest(in)
	if d.Counts.SavedDollars == nil || *d.Counts.SavedDollars != 420 {
		t.Fatalf("saved dollars = %v, want 420 (2.8 h x 150)", d.Counts.SavedDollars)
	}
	if !strings.Contains(d.Sections[2].Summary, "$420") {
		t.Fatalf("saved summary = %q, want the dollar figure", d.Sections[2].Summary)
	}
	in.DBAHourRateUSD = -5
	if d := BuildDigest(in); d.Counts.SavedDollars != nil {
		t.Fatal("a negative rate priced the hours")
	}
}

// Boundary: equal content at a different time has the same hash; one
// changed count changes it.
func TestDigestHash_IgnoresTimeButNotCounts(t *testing.T) {
	a := BuildDigest(happyInput())
	later := happyInput()
	later.Window = Window{Start: digestWindow.End, End: digestWindow.End.Add(24 * time.Hour)}
	b := BuildDigest(later)
	if a.Hash != b.Hash {
		t.Fatalf("equal content hashed %s and %s", a.Hash, b.Hash)
	}
	withHeadline := a
	withHeadline.Headline = "A model wrote this"
	if digestHash(withHeadline) != a.Hash {
		t.Fatal("the headline changed the hash: a model's wording would resend the digest")
	}
	more := happyInput()
	more.Inbox = &InboxCounts{Total: 3, ByKind: map[string]int{"change_approval": 3}}
	if c := BuildDigest(more); c.Hash == a.Hash {
		t.Fatal("a changed inbox count kept the same hash")
	}
}

// Nil/empty: nothing happened is an empty digest saying so.
func TestBuildDigest_NothingChanged(t *testing.T) {
	d := BuildDigest(DigestInput{Database: "prod", Window: digestWindow,
		Inbox: &InboxCounts{ByKind: map[string]int{}}})
	if !d.Empty() {
		t.Fatalf("digest %+v is not empty", d.Counts)
	}
	if d.Headline != "Nothing changed" {
		t.Fatalf("headline = %q, want Nothing changed", d.Headline)
	}
	if len(d.Sections) != 5 {
		t.Fatalf("%d sections, want all five even when empty", len(d.Sections))
	}
	if !strings.Contains(d.Text(), "Nothing changed") {
		t.Fatalf("text = %q, want it to say Nothing changed", d.Text())
	}
}

// Error propagation: a source that could not be read is a named gap, not
// a zero, and the digest is not empty because of it.
func TestBuildDigest_UnreadSourcesAreGaps(t *testing.T) {
	d := BuildDigest(DigestInput{Database: "prod", Window: digestWindow,
		OutcomesErr: errors.New("read outcomes: prod: not connected"),
		InboxErr:    errors.New("inbox down"), HeldBackErr: errors.New("ledger down")})
	gaps := strings.Join(d.Gaps, "|")
	for _, want := range []string{"outcome ledger", "inbox", "held-back"} {
		if !strings.Contains(gaps, want) {
			t.Errorf("gaps = %q, want %q named", gaps, want)
		}
	}
	if d.Empty() {
		t.Fatal("a digest with unread sources is empty: it would say Nothing changed")
	}
	if strings.Contains(d.Text(), "not connected") {
		t.Fatalf("text = %q leaks the raw error", d.Text())
	}
	if !strings.Contains(d.Text(), "could not be read") {
		t.Fatalf("text = %q, want the gap said", d.Text())
	}
}

// A lower-bound inbox count says so.
func TestBuildDigest_InboxLowerBound(t *testing.T) {
	in := happyInput()
	in.Inbox.LowerBound = true
	d := BuildDigest(in)
	if !d.Counts.NeedsYouLowerBound || !strings.Contains(d.Sections[3].Summary, "at least") {
		t.Fatalf("needs-you = %q, want an at-least count", d.Sections[3].Summary)
	}
}

func TestDeterministicHeadline_Forms(t *testing.T) {
	for _, tc := range []struct {
		c    DigestCounts
		want string
	}{
		{DigestCounts{}, "Nothing changed"},
		{DigestCounts{Changes: 1, Verified: 1}, "1 change, 1 verified"},
		{DigestCounts{NeedsYou: 1}, "0 changes, 1 needs you"},
		{DigestCounts{HeldBack: 2}, "0 changes, 2 held back"},
		{DigestCounts{Changes: 6, Verified: 5, Reverted: 1, NeedsYou: 2},
			"6 changes, 5 verified, 1 reverted, 2 need you"},
	} {
		if got := DeterministicHeadline(tc.c); got != tc.want {
			t.Errorf("DeterministicHeadline(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

// The message names the database, the window and every section.
func TestDigestText_HasEverySection(t *testing.T) {
	text := BuildDigest(happyInput()).Text()
	for _, want := range []string{"prod", "6 changes, 5 verified", "Did", "Verified or reverted",
		"Saved", "Needs you", "Could not do and why", "Index created"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

// The identifiers a headline may not repeat: target objects and the
// identifier-shaped words of the change descriptions.
func TestDigestForbiddenTerms(t *testing.T) {
	in := happyInput()
	in.Outcomes.Rows[0].What = "Index created on public.order_items"
	terms := "," + strings.Join(forbiddenTerms(BuildDigest(in)), ",") + ","
	for _, want := range []string{"public.orders", "orders", "public.order_items",
		"order_items"} {
		if !strings.Contains(terms, ","+want+",") {
			t.Errorf("forbidden terms %q lack %q", terms, want)
		}
	}
	if strings.Contains(strings.ToLower(terms), ",index,") {
		t.Errorf("forbidden terms %q hold a plain word", terms)
	}
}
