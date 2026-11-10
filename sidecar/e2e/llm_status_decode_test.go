//go:build e2e

package e2e

import (
	"errors"
	"strings"
	"testing"
)

// No concurrent-access case: decodeLLMClients is a pure function of its
// input and holds no shared state.

func TestDecodeLLMClients(t *testing.T) {
	body := `{"clients":{"general":{"enabled":true,"tokens_used":150,` +
		`"token_budget":9000,"budget_exhausted":false,"circuit_open":true}},` +
		`"any_exhausted":false}`
	got, err := decodeLLMClients([]byte(body))
	if err != nil {
		t.Fatalf("decode a keyed status body: %v", err)
	}
	st, ok := got["general"]
	if len(got) != 1 || !ok || !st.Enabled || st.TokensUsed != 150 ||
		st.TokenBudget != 9000 || !st.CircuitOpen || st.Exhausted {
		t.Fatalf("decoded clients = %+v, want only general: enabled, 150 of 9000, "+
			"circuit open, not exhausted", got)
	}
}

func TestDecodeLLMClientsNotWired(t *testing.T) {
	// The handler's answer when the process has no LLM runtime, and the
	// zero cases: an empty object body, a null and a missing clients key.
	for _, body := range []string{
		`{"clients":[],"any_exhausted":false}`,
		`{"clients":null}`,
		`{}`,
	} {
		got, err := decodeLLMClients([]byte(body))
		if !errors.Is(err, errLLMNotWired) || got != nil {
			t.Errorf("decode %s = %v, %v; want errLLMNotWired and no map",
				body, got, err)
		}
	}
}

func TestDecodeLLMClientsMalformed(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`not json`, "decode status body"},
		{``, "decode status body"},
		{`{"clients":{"general":{"tokens_used":"many"}}}`, "decode clients"},
	} {
		got, err := decodeLLMClients([]byte(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) ||
			errors.Is(err, errLLMNotWired) || got != nil {
			t.Errorf("decode %q = %v, %v; want a %q error", tc.body, got, err, tc.want)
		}
	}
}
