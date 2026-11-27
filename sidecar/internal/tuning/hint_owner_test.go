package tuning

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/tuner"
)

// tuning-02: the tuner refuses a statement that already carries a hint,
// pg_sage's own (ErrHintExists) or an operator's (ErrForeignHint). Both are
// duplicates of a hint in place, not invalid proposals.
//
// No concurrent access tests: judgeHint holds no state of its own; the
// tuner serializes the durable check (internal/tuner).
func TestJudge_QueryHintOnHintedStatementIsDuplicate(t *testing.T) {
	for _, refusal := range []error{tuner.ErrHintExists, tuner.ErrForeignHint} {
		h := newHarness(t)
		h.hints.available = true
		h.hints.err = fmt.Errorf("%w: queryid 101", refusal)
		j := judgeOne(t, h, nil, hintProposal(101))
		if j.Verdict != VerdictRejected || j.Reason != ReasonDuplicate {
			t.Fatalf("%v: judged %+v, want rejected as duplicate", refusal, j)
		}
		if !strings.Contains(j.Detail, "queryid 101") {
			t.Fatalf("%v: detail %q does not carry the tuner's reason", refusal, j.Detail)
		}
		if j.Finding != nil {
			t.Fatalf("%v: a refused hint produced a finding: %+v", refusal, j.Finding)
		}
	}
}
