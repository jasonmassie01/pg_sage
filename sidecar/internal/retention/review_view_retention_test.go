package retention

import (
	"strings"
	"testing"
)

// review-01b: sage.review_view has no purge rule of its own; its rows go
// with their review run (ON DELETE CASCADE), which reviewRunRule purges.
// No concurrent access tests: a static map lookup.
func TestRetentionExemptions_ReviewViewGoesWithItsRun(t *testing.T) {
	reason := retentionExemptions["review_view"]
	if !strings.Contains(reason, "ON DELETE CASCADE") {
		t.Fatalf("review_view exemption = %q, want the cascade named", reason)
	}
	for _, r := range purgeRules(allDays(30)) {
		if r.table == "review_view" {
			t.Fatalf("review_view has a purge rule %+v and an exemption", r)
		}
	}
}
