package ask

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-09: Ask Sage's system prompt carries the shared untrusted-data rule,
// not only its own wording, so the rule cannot drift per package.
func TestSystemPromptCarriesSharedUntrustedDataRule(t *testing.T) {
	for _, c := range []Caller{{}, {MayPropose: true}} {
		got := systemPrompt("orders", c)
		if n := strings.Count(got, llm.UntrustedDataRule); n != 1 {
			t.Errorf("caller %+v: rule appears %d times, want 1:\n%s", c, n, got)
		}
		if !strings.Contains(got, "orders") {
			t.Errorf("caller %+v: prompt lost the database name", c)
		}
	}
}
