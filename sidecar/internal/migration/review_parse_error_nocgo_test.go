//go:build !cgo

package migration

import (
	"context"
	"strings"
	"testing"
)

// No concurrent access or state tests: parseRefusal is a pure function.

// Without the parser there is no refusal to report: the note of an
// unrecognized statement is the fixed text alone, and the report names
// the missing parser instead.
func TestReviewNoteWithoutParserNamesNoRefusal(t *testing.T) {
	sql := "ALTER TABLE t ADD COLUMN"
	if got := parseRefusal(sql); got != "" {
		t.Fatalf("parseRefusal without a parser = %q, want empty", got)
	}
	rep, err := Review(context.Background(), nil, sql, ReviewOptions{})
	if err != nil || len(rep.Statements) != 1 {
		t.Fatalf("Review: %+v %v", rep, err)
	}
	st := rep.Statements[0]
	if st.Status != StatementUnrecognized || st.Verdict != VerdictReview {
		t.Fatalf("status %s verdict %s, want unrecognized review", st.Status, st.Verdict)
	}
	want := "No DDL safety rule matches this statement and it is not a known " +
		"safe form: unrecognized DDL is not proven safe."
	if st.Note != want {
		t.Fatalf("note %q, want %q", st.Note, want)
	}
	if !strings.Contains(strings.Join(rep.Notes, " "), "this build has no SQL parser") {
		t.Fatalf("report notes %q do not name the missing parser", rep.Notes)
	}
}
