package optimizer

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Post-test audit additions: inputs the first round did not exercise.

// Function-call keys carry commas inside their argument list; the key
// splitter must not cut them, and argument order is part of the idea.
func TestCandidateShape_FunctionKeysWithCommas(t *testing.T) {
	a := mustShape(t, "CREATE INDEX i ON t ((coalesce(a, b)), c)")
	b := mustShape(t, "CREATE INDEX j ON t (coalesce( a,b ), c)")
	c := mustShape(t, "CREATE INDEX i ON t ((coalesce(b, a)), c)")
	if len(a.Keys) != 2 || a.Keys[0] != "coalesce(a,b)" {
		t.Fatalf("keys = %q, want [coalesce(a,b) c]", a.Keys)
	}
	if !a.sameIdea(b) || a.sameIdea(c) {
		t.Fatalf("coalesce(a, b): same spelling %t, swapped args %t", a.sameIdea(b),
			a.sameIdea(c))
	}
}

// Non-ASCII identifiers: quoted ones keep their quotes and case, and an
// unquoted one compares equal to itself quoted (it folds to itself).
func TestCandidateShape_NonASCIIIdentifiers(t *testing.T) {
	quoted := mustShape(t, `CREATE INDEX i ON t ("Größe") INCLUDE ("ñame")`)
	if quoted.Keys[0] != `"Größe"` {
		t.Fatalf("key = %q, want the quoted mixed-case name", quoted.Keys[0])
	}
	if mustShape(t, `CREATE INDEX i ON t (größe)`).sameIdea(quoted) {
		t.Fatal(`"Größe" and größe are different columns`)
	}
}

// A prompt line cut for length stays valid UTF-8. (String literals are
// redacted to '?' before the prompt, so the long text is an identifier.)
func TestTableMemory_PromptLineTruncationKeepsUTF8(t *testing.T) {
	long := `CREATE INDEX i ON public.ai_claims ("` + strings.Repeat("é", 400) + `")`
	lines := testMemory(newMemStore(storedRejection(t, long, time.Minute)), nil).
		view(context.Background(), memTable()).promptLines()
	if len(lines) != 1 || len(lines[0]) > maxRejectionPromptLine ||
		!utf8.ValidString(lines[0]) || !strings.Contains(lines[0], "...") {
		t.Fatalf("truncated line: len=%d valid=%t %q", len(lines[0]),
			utf8.ValidString(lines[0]), lines[0])
	}
}
