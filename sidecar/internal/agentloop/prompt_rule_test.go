package agentloop

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-09: the loop's system message carries the untrusted-data rule exactly
// once, whether or not the caller's own system text already appends it.
func TestSystemPromptCarriesTheRuleOnce(t *testing.T) {
	final := Final{Name: "answer", Description: "the answer"}
	for _, system := range []string{
		"You are a DBA.",
		"You are a DBA.\n" + llm.UntrustedDataRule,
		"",
	} {
		for _, p := range []Protocol{ProtocolNative, ProtocolJSON} {
			got := systemPrompt(Config{System: system, Final: final}, p)
			if n := strings.Count(got, llm.UntrustedDataRule); n != 1 {
				t.Errorf("system %q protocol %s: rule appears %d times, want 1",
					system, p, n)
			}
		}
	}
}
