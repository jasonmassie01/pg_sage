package llm

import (
	"strings"
	"sync"
	"testing"
)

// SanitizeForLLMStrict is SanitizeForLLM with numeric redaction forced on
// (evid-02c): the workload pack's model tier never depends on the
// process-wide switch.

func TestSanitizeForLLMStrict_RedactsNumbersWithTheSwitchOff(t *testing.T) {
	defer SetRedactNumericLiterals(true)
	SetRedactNumericLiterals(false)
	in := "SELECT * FROM cards WHERE pan = 4111111111111111 AND name = 'ann' -- hi"
	got := SanitizeForLLMStrict(in)
	if strings.Contains(got, "4111111111111111") || strings.Contains(got, "ann") ||
		strings.Contains(got, "hi") {
		t.Fatalf("strict sanitize kept a secret: %q", got)
	}
	if want := "SELECT * FROM cards WHERE pan = ? AND name = '?'"; strings.TrimSpace(got) !=
		want {
		t.Fatalf("strict sanitize = %q, want %q", got, want)
	}
	// The process switch is unchanged, and SanitizeForLLM keeps the number.
	if RedactNumericLiteralsEnabled() {
		t.Fatal("SanitizeForLLMStrict turned the process switch on")
	}
	if plain := SanitizeForLLM(in); !strings.Contains(plain, "4111111111111111") {
		t.Fatalf("SanitizeForLLM with the switch off = %q, want the number kept", plain)
	}
}

func TestSanitizeForLLMStrict_SwitchOnMatchesSanitizeForLLM(t *testing.T) {
	SetRedactNumericLiterals(true)
	for _, in := range []string{
		"SELECT 1 WHERE a = 1234567 AND b = $$x$$",
		`{"Filter": "(id = 9876543)", "Plan Rows": 12345678}`,
		`{"broken": "x = 'y'`,
		"",
	} {
		if got, want := SanitizeForLLMStrict(in), SanitizeForLLM(in); got != want {
			t.Errorf("strict(%q) = %q, plain = %q", in, got, want)
		}
	}
}

func TestSanitizeForLLMStrict_PlanJSONNumbersRedactedInStrings(t *testing.T) {
	defer SetRedactNumericLiterals(true)
	SetRedactNumericLiterals(false)
	got := SanitizeForLLMStrict(`{"Filter": "(acct = 123456789)", "Plan Rows": 1234567}`)
	if strings.Contains(got, "123456789") || !strings.Contains(got, "1234567}") {
		t.Fatalf("strict plan sanitize = %q", got)
	}
}

func TestSanitizeForLLMStrict_ConcurrentWithSwitchFlips(t *testing.T) {
	defer SetRedactNumericLiterals(true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(on bool) { defer wg.Done(); SetRedactNumericLiterals(on) }(i%2 == 0)
		go func() {
			defer wg.Done()
			if got := SanitizeForLLMStrict("WHERE x = 55555555"); strings.Contains(got,
				"55555555") {
				t.Errorf("strict sanitize kept the number during a flip: %q", got)
			}
		}()
	}
	wg.Wait()
}
