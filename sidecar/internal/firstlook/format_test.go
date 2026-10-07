package firstlook

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// No concurrent access tests here: pct and the rules it feeds are pure.

// A share between 99% and 100% shows one decimal, so a nearly exhausted
// sequence never reads "100% used"; only a real 100% does.
func TestPctNearFullShowsOneDecimal(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.996515, "99.7%"},
		{0.99, "99.0%"},
		{0.99949, "99.9%"},
		{0.99999, "99.9%"}, // rounds to 100.0 but is not exhausted
		{math.Nextafter(1, 0), "99.9%"},
		{1, "100%"},
		{1.2, "120%"},
		{0.989, "99%"},
		{0.93, "93%"},
		{0.5, "50%"},
		{0, "0%"},
	} {
		if got := pct(tc.in); got != tc.want {
			t.Errorf("pct(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The verification case: a bigint sequence at 2,140,000,000 feeding an int
// column is 99.65% used; the title says 99.7%, not 100%.
func TestSequenceTitleNearExhaustionIsNotRoundedToFull(t *testing.T) {
	s := seq("big_seq", i64(2_140_000_000), 1, math.MaxInt64, 1, "integer")
	items, _ := SequenceRunway([]Sequence{s}, DefaultThresholds())
	if len(items) != 1 || items[0].Severity != SeverityCritical {
		t.Fatalf("items = %+v, want one critical", items)
	}
	if !strings.Contains(items[0].Title, "99.7% used") ||
		strings.Contains(items[0].Title, "100%") {
		t.Fatalf("title %q, want 99.7%% used", items[0].Title)
	}
	full := seq("full", i64(math.MaxInt32), 1, math.MaxInt64, 1, "integer")
	items, _ = SequenceRunway([]Sequence{full}, DefaultThresholds())
	if len(items) != 1 || !strings.Contains(items[0].Title, "100% used") {
		t.Fatalf("exhausted sequence items = %+v, want 100%% used", items)
	}
}

// docs/quickstart.md states the table size the dead-tuple estimate starts
// at; the number comes from DefaultThresholds, so the two cannot drift.
func TestQuickstartDeadTupleMinSizeMatchesThreshold(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/quickstart.md")
	if err != nil {
		t.Fatalf("read quickstart: %v", err)
	}
	minBytes := DefaultThresholds().BloatMinBytes
	if minBytes <= 0 || minBytes%(1<<20) != 0 {
		t.Fatalf("BloatMinBytes = %d, want a positive whole number of MB", minBytes)
	}
	want := fmt.Sprintf("at least %d MB", minBytes>>20)
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n") // CRLF checkouts
	for _, line := range strings.Split(doc, "\n\n") {
		if strings.Contains(line, "dead tuples") {
			if !strings.Contains(strings.Join(strings.Fields(line), " "), want) {
				t.Fatalf("the dead-tuple bullet does not say %q:\n%s", want, line)
			}
			return
		}
	}
	t.Fatal("docs/quickstart.md has no dead-tuple bullet")
}
