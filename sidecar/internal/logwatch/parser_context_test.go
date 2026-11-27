package logwatch

import "testing"

// rca-07b: the CONTEXT field (csvlog column 18, jsonlog "context") names
// the relation a deadlocked backend was working on.

// No concurrent access tests: the parsers are pure functions.

func TestParseCSVLogLine_ContextFromColumn18(t *testing.T) {
	want := `while updating tuple (0,2) in relation "accounts"`
	e, err := ParseCSVLogLine(makeCSVRecord(map[int]string{18: want}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Context != want || csvColContext != 18 {
		t.Errorf("Context = %q (column %d), want %q from column 18", e.Context,
			csvColContext, want)
	}
	if e.Query != "SELECT 1" || e.Detail != "detail text" {
		t.Errorf("neighbouring columns moved: query %q detail %q", e.Query, e.Detail)
	}
}

// Nil/empty: an empty column leaves the context empty.
func TestParseCSVLogLine_NoContext(t *testing.T) {
	e, err := ParseCSVLogLine(makeCSVRecord(nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Context != "" {
		t.Errorf("Context = %q, want empty", e.Context)
	}
}

func TestParseJSONLogLine_Context(t *testing.T) {
	line := `{"timestamp":"2026-11-08 20:07:05.449 UTC","pid":4242,` +
		`"error_severity":"ERROR","state_code":"40P01","message":"deadlock detected",` +
		`"detail":"Process 4242 waits for ShareLock on transaction 745; ` +
		`blocked by process 4243.","context":"while updating tuple (0,2) in ` +
		`relation \"accounts\""}`
	e, err := ParseJSONLogLine([]byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.Context != `while updating tuple (0,2) in relation "accounts"` {
		t.Errorf("Context = %q", e.Context)
	}
	parts, unparsed := ParseDeadlockDetail(e.Detail, e.Context)
	if len(parts) != 1 || parts[0].Relation != "accounts" || unparsed != 0 {
		t.Errorf("participants = %+v, unparsed = %d", parts, unparsed)
	}
}
