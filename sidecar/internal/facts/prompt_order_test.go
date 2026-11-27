package facts

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// facts-11: the prompt keeps the facts that matter most for WAL and
// vacuum advice when the budget cuts, and says how many it cut.
// No concurrent access tests here: PromptLines is a pure function of its
// arguments and shares no state.

// shortFixtures are n short test-fixture facts with ids from..from+n-1.
func shortFixtures(from, n int) []Fact {
	var out []Fact
	for i := from; i < from+n; i++ {
		out = append(out, confirmed(int64(i), TypeTestFixture, KindSchema,
			fmt.Sprintf("t%d", i), nil))
	}
	return out
}

func slotFact(id int64) Fact {
	return confirmed(id, TypeSlotConsumer, KindSlot, "cdc_orders",
		map[string]string{"consumer": "debezium"})
}

func promptChars(lines []string) int {
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	return total
}

func hasFactID(line string, id int64) bool {
	return strings.HasPrefix(line, fmt.Sprintf("- fact #%d ", id))
}

// The line cap: twelve fact lines, then the note as line 13.
func TestPromptLinesNoteCountsFactsCutByTheLineCap(t *testing.T) {
	facts := append(shortFixtures(1, 30), slotFact(31))
	lines := PromptLines(facts, bindNow)
	if len(lines) != maxPromptLines+1 {
		t.Fatalf("want 12 facts and the note, got %d lines:\n%s", len(lines),
			strings.Join(lines, "\n"))
	}
	if !hasFactID(lines[0], 31) {
		t.Fatalf("the slot fact must lead, line 1 is %q", lines[0])
	}
	for i, l := range lines[1:maxPromptLines] {
		if !hasFactID(l, int64(i+1)) {
			t.Fatalf("line %d is %q, want fact #%d (ids order ties)", i+2, l, i+1)
		}
	}
	if got := lines[maxPromptLines]; got != "- 19 more confirmed facts omitted." {
		t.Fatalf("note %q", got)
	}
	if n := promptChars(lines); n > maxPromptChars {
		t.Fatalf("%d chars exceed the budget", n)
	}
}

// The character budget: thirty index-ownership facts and one slot fact.
// The slot fact survives, and the note fits inside maxPromptChars.
func TestPromptLinesKeepSlotFactAmongManyIndexFacts(t *testing.T) {
	var facts []Fact
	for i := 1; i <= 30; i++ {
		facts = append(facts, confirmed(int64(i), TypeAppMigrations, KindIndex,
			fmt.Sprintf("public.idx_orders_customer_%02d", i), nil))
	}
	facts = append(facts, slotFact(31))
	lines := PromptLines(facts, bindNow)
	if len(lines) < 3 || !hasFactID(lines[0], 31) {
		t.Fatalf("the slot fact must lead:\n%s", strings.Join(lines, "\n"))
	}
	kept := len(lines) - 1
	want := fmt.Sprintf("- %d more confirmed facts omitted.", 31-kept)
	if lines[kept] != want {
		t.Fatalf("last line %q, want %q", lines[kept], want)
	}
	for i := 1; i < kept; i++ {
		if !hasFactID(lines[i], int64(i)) {
			t.Fatalf("line %d is %q, want index fact #%d", i+1, lines[i], i)
		}
	}
	if n := promptChars(lines); n > maxPromptChars {
		t.Fatalf("%d chars exceed the budget of %d", n, maxPromptChars)
	}
	// The cut is real: one more fact line would not have fit with the note.
	next := clip(promptLine(facts[kept-1]), maxPromptLine)
	if promptChars(lines)+len(next)+1 <= maxPromptChars && kept < maxPromptLines {
		t.Fatalf("fact #%d was cut although it fits:\n%s", kept, strings.Join(lines, "\n"))
	}
}

func TestPromptLinesExactlyTwelveFactsHaveNoNote(t *testing.T) {
	lines := PromptLines(shortFixtures(1, maxPromptLines), bindNow)
	if len(lines) != maxPromptLines {
		t.Fatalf("%d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if strings.Contains(l, "omitted") {
			t.Fatalf("no fact was cut, yet: %q", l)
		}
	}
}

func TestPromptLinesThirteenFactsNoteOneOmitted(t *testing.T) {
	lines := PromptLines(shortFixtures(1, maxPromptLines+1), bindNow)
	if len(lines) != maxPromptLines+1 ||
		lines[maxPromptLines] != "- 1 more confirmed fact omitted." {
		t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
	}
	if !hasFactID(lines[maxPromptLines-1], maxPromptLines) {
		t.Fatalf("the last kept fact is %q", lines[maxPromptLines-1])
	}
}

func TestPromptLinesNoFactsNoNote(t *testing.T) {
	if got := PromptLines(nil, bindNow); got != nil {
		t.Fatalf("nil facts: %q", got)
	}
	if got := PromptLines([]Fact{}, bindNow); len(got) != 0 {
		t.Fatalf("empty facts: %q", got)
	}
	if got := PromptLines([]Fact{{}}, bindNow); len(got) != 0 {
		t.Fatalf("a zero fact is not confirmed: %q", got)
	}
}

func TestPromptLinesOrderByTypePriorityThenID(t *testing.T) {
	window := confirmed(7, TypeTableWindow, KindTable, "app.orders",
		map[string]string{"kind": "maintenance", "window": "Sun 02:00-04:00 UTC"})
	facts := []Fact{
		confirmed(1, TypeTestFixture, KindSchema, "tmp_*", nil),
		confirmed(2, TypeAppMigrations, KindIndex, "public.idx_a", nil),
		confirmed(3, TypeAppendOnly, KindTable, "app.audit", nil),
		window,
		confirmed(5, TypeSlotConsumer, KindSlot, "b", map[string]string{"consumer": "x"}),
		confirmed(6, TypeAppMigrations, KindIndex, "public.idx_b", nil),
		confirmed(4, TypeSlotConsumer, KindSlot, "a", map[string]string{"consumer": "x"}),
	}
	before := fmt.Sprint(facts)
	lines := PromptLines(facts, bindNow)
	want := []int64{4, 5, 7, 3, 2, 6, 1}
	if len(lines) != len(want) {
		t.Fatalf("lines:\n%s", strings.Join(lines, "\n"))
	}
	for i, id := range want {
		if !hasFactID(lines[i], id) {
			t.Fatalf("line %d is %q, want fact #%d", i+1, lines[i], id)
		}
	}
	if fmt.Sprint(facts) != before {
		t.Fatal("PromptLines reordered the caller's slice")
	}
}

// Expired, proposed and rejected facts are neither shown nor counted.
func TestPromptLinesUnboundFactsAreNotCountedAsOmitted(t *testing.T) {
	facts := shortFixtures(1, maxPromptLines)
	past := bindNow.Add(-time.Minute)
	for i := 0; i < 5; i++ {
		p := slotFact(int64(100 + i))
		p.Status = StatusProposed
		e := slotFact(int64(200 + i))
		e.ExpiresAt = &past
		r := slotFact(int64(300 + i))
		r.Status = StatusRejected
		facts = append(facts, p, e, r)
	}
	lines := PromptLines(facts, bindNow)
	joined := strings.Join(lines, "\n")
	if len(lines) != maxPromptLines || strings.Contains(joined, "omitted") ||
		strings.Contains(joined, "cdc_orders") {
		t.Fatalf("lines:\n%s", joined)
	}
}

// Facts the object filter leaves out are not "omitted"; a single-fact call
// (tuning packets) gets its line and no note.
func TestPromptLinesFilteredFactsAreNotCountedAsOmitted(t *testing.T) {
	var facts []Fact
	for i := 1; i <= 20; i++ {
		facts = append(facts, confirmed(int64(i), TypeAppMigrations, KindTable,
			fmt.Sprintf("app.t%02d", i), nil))
	}
	lines := PromptLines(facts, bindNow, "app.t07")
	if len(lines) != 1 || !hasFactID(lines[0], 7) {
		t.Fatalf("filtered lines %q", lines)
	}
	single := PromptLines(facts[2:3], bindNow, "app.t03")
	if len(single) != 1 || !hasFactID(single[0], 3) {
		t.Fatalf("single-fact call %q", single)
	}
}

// Invalid input: a fact of an unknown type still renders, after the known
// priorities, and never displaces a slot fact.
func TestPromptLinesUnknownTypeSortsLast(t *testing.T) {
	odd := confirmed(1, Type("mystery"), KindTable, "app.x", nil)
	lines := PromptLines([]Fact{odd, slotFact(2)}, bindNow)
	if len(lines) != 2 || !hasFactID(lines[0], 2) || !hasFactID(lines[1], 1) {
		t.Fatalf("lines %q", lines)
	}
}

// fixtureOfLine is a test-fixture fact whose prompt line is exactly n chars.
func fixtureOfLine(t *testing.T, id int64, n int) Fact {
	t.Helper()
	for pad := 1; pad < maxPromptLine; pad++ {
		f := confirmed(id, TypeTestFixture, KindSchema, strings.Repeat("s", pad), nil)
		if got := len(clip(promptLine(f), maxPromptLine)); got == n {
			return f
		} else if got > n {
			break
		}
	}
	t.Fatalf("no fixture renders to a %d-char line", n)
	return Fact{}
}

// The note must fit: when the fact lines leave less room than the note
// needs, the last fact line gives way and is counted as omitted too.
func TestPromptLinesNoteDisplacesALastLineThatLeavesNoRoom(t *testing.T) {
	var facts []Fact
	for i := int64(1); i <= 4; i++ {
		facts = append(facts, fixtureOfLine(t, i, 299)) // 4 x 300 chars used
	}
	// Line 5 leaves 10 chars, less than the note needs; line 6 cannot fit.
	facts = append(facts, fixtureOfLine(t, 5, 289), fixtureOfLine(t, 6, 200))
	lines := PromptLines(facts, bindNow)
	if len(lines) != 5 || lines[4] != "- 2 more confirmed facts omitted." {
		t.Fatalf("want 4 facts and a note of 2, got:\n%s", strings.Join(lines, "\n"))
	}
	for i := 0; i < 4; i++ {
		if !hasFactID(lines[i], int64(i+1)) {
			t.Fatalf("line %d is %q", i+1, lines[i])
		}
	}
	if n := promptChars(lines); n > maxPromptChars {
		t.Fatalf("%d chars exceed the budget of %d", n, maxPromptChars)
	}
	// Without line 6 nothing is cut: five lines of 1,490 chars and no note.
	if got := PromptLines(facts[:5], bindNow); len(got) != 5 || hasNote(got) {
		t.Fatalf("five fitting facts:\n%s", strings.Join(got, "\n"))
	}
}

func hasNote(lines []string) bool {
	return len(lines) > 0 && strings.HasSuffix(lines[len(lines)-1], "omitted.")
}
