package llm

import (
	"testing"
	"time"
)

// llm-09 review: inputs that once stalled the scanner or slipped past it.

// redactWithin runs RedactNumericLiterals with a deadline, so a scanner
// that stops advancing fails the test instead of hanging the suite.
func redactWithin(t *testing.T, in string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- RedactNumericLiterals(in) }()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatalf("RedactNumericLiterals(%q) did not return: the scan stopped advancing", in)
		return ""
	}
}

func TestRedactNumericLiteralsSignOrPointBeforeWord(t *testing.T) {
	// A sign or point before digits that run into letters (".5s", "-5px")
	// used to spin forever: the word reader consumed nothing.
	cases := []struct{ in, want string }{
		{"duration .5s exceeded", "duration .5s exceeded"},
		{"x = -1abc", "x = -1abc"},
		{"margin (-5px)", "margin (-5px)"},
		{"x = .5e", "x = .5e"},
		{"x = +7d AND y = 1234567", "x = +7d AND y = ?"},
		{"-1x", "-1x"},
		{".9", ".9"},
	}
	for _, c := range cases {
		if got := redactWithin(t, c.in); got != c.want {
			t.Errorf("RedactNumericLiterals(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactLiteralsForLLMLogWithUnitsReturns(t *testing.T) {
	// The rca Tier 2 path: log text with durations and sizes.
	prev := RedactNumericLiteralsEnabled()
	SetRedactNumericLiterals(true)
	t.Cleanup(func() { SetRedactNumericLiterals(prev) })
	in := "canceling statement after .5s: Key (id)=(4111111111111111) (-5px)"
	want := "canceling statement after .5s: Key (id)=(?) (-5px)"
	done := make(chan string, 1)
	go func() { done <- RedactLiteralsForLLM(in) }()
	select {
	case got := <-done:
		if got != want {
			t.Fatalf("RedactLiteralsForLLM = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RedactLiteralsForLLM did not return")
	}
}

func TestRedactNumericLiteralsArrayAndLineBreaks(t *testing.T) {
	cases := []struct{ in, want string }{
		{"WHERE ids = ARRAY[123456789, 5]", "WHERE ids = ARRAY[?, 5]"},
		{"WHERE ids <> ARRAY[[1234567], [2]]", "WHERE ids <> ARRAY[[?], [2]]"},
		{"SELECT ARRAY[1234567] AS a", "SELECT ARRAY[1234567] AS a"},
		{"WHERE 123456789\n= ssn", "WHERE ?\n= ssn"},
		{"WHERE 123456789\r\n  <> ssn", "WHERE ?\r\n  <> ssn"},
		{"WHERE ssn\n=\n123456789", "WHERE ssn\n=\n?"},
	}
	for _, c := range cases {
		if got := redactWithin(t, c.in); got != c.want {
			t.Errorf("RedactNumericLiterals(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
