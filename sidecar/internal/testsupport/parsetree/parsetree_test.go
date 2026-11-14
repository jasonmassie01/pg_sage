package parsetree

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Available must agree with what the planner's parser does in this build:
// a test that runs must be able to plan, and one that skips must not.
func TestAvailableMatchesTheParser(t *testing.T) {
	_, err := sqlast.ParsePlannableAlter(
		"ALTER TABLE public.users ALTER COLUMN email SET NOT NULL")
	unavailable := errors.Is(err, sqlast.ErrUnavailable)
	if Available() == unavailable {
		t.Fatalf("Available() = %v but the parser error is %v", Available(), err)
	}
	if !unavailable && err != nil {
		t.Fatalf("a build with the parser refused a plannable ALTER: %v", err)
	}
}

// Require runs the test body exactly when the parser is linked in, and
// otherwise reports a skip with the reason.
func TestRequireSkipsExactlyWithoutTheParser(t *testing.T) {
	ran := false
	t.Run("needs_parse_tree", func(t *testing.T) {
		Require(t)
		ran = true
	})
	if ran != Available() {
		t.Fatalf("body ran = %v, Available() = %v", ran, Available())
	}
}
