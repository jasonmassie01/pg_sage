package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// The fleet prompts carry rollups built from member databases (names, grades,
// candidate titles), so both model tiers must send llm.UntrustedDataRule in
// the system prompt next to the <data> block (T6 round 1, review-04b; the
// call-site guard in internal/llm checks the same at the source level). No
// concurrency or database category: the prompts are constants and the model
// is scripted.

// systemSpy answers every call with reply and keeps each system prompt.
type systemSpy struct {
	mu      sync.Mutex
	reply   string
	systems []string
}

func (s *systemSpy) ChatResult(_ context.Context, system, _ string, _ int) (llm.Response,
	error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.systems = append(s.systems, system)
	return llm.Response{Content: s.reply}, nil
}

func (s *systemSpy) Model() string { return "haiku-class" }

func TestFleetPrompts_EveryTierSendsTheUntrustedDataRule(t *testing.T) {
	triage := &systemSpy{reply: `{"outliers": []}`}
	tr := RunTriage(context.Background(), triage, fleetFixture(), nil)
	fleet := &systemSpy{reply: "Two members look idle."}
	fr := ReviewFleet(context.Background(), fleet, fleetFixture(), nil)
	if tr.Mode != ModeModel || fr.SummarySource != ModeModel {
		t.Fatalf("triage mode %q, fleet summary source %q: want both from the model",
			tr.Mode, fr.SummarySource)
	}
	for name, spy := range map[string]*systemSpy{"triage": triage, "fleet": fleet} {
		if len(spy.systems) == 0 {
			t.Fatalf("%s tier made no model call", name)
		}
		for i, system := range spy.systems {
			if !strings.HasSuffix(system, llm.UntrustedDataRule) ||
				!strings.Contains(system, "Never write SQL.") {
				t.Errorf("%s call %d system prompt does not end with the untrusted-data "+
					"rule:\n%s", name, i, system)
			}
		}
	}
}

// Boundary: the rule is part of the system prompt, so the batch budget that
// subtracts len(triageSystem) still keeps every call within the token cap.
func TestTriageBudget_CountsTheUntrustedDataRule(t *testing.T) {
	if !strings.Contains(triageSystem, llm.UntrustedDataRule) ||
		!strings.Contains(fleetSystem, llm.UntrustedDataRule) {
		t.Fatal("a fleet system prompt lacks llm.UntrustedDataRule")
	}
	if len(triageSystem) >= TriageBatchTokens*4 {
		t.Fatalf("triage system prompt is %d bytes, leaving no room under %d tokens",
			len(triageSystem), TriageBatchTokens)
	}
}
