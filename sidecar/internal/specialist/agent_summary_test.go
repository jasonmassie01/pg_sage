package specialist

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// summary_for_agent (mcp-02, contract revision 1.2.0): the result in the
// shape generalist agents' quality gates check (root cause, the cited
// evidence with its numbers, specific actions), plus the first failing
// why-not link when no action can be requested.

func summaryOf(t *testing.T, h *harness, id sre.UUID) AgentSummary {
	t.Helper()
	r, err := h.svc.Result(context.Background(), proposer, "orders", string(id))
	if err != nil {
		t.Fatal(err)
	}
	if r.SummaryForAgent == nil {
		t.Fatalf("result without summary_for_agent: %+v", r)
	}
	return *r.SummaryForAgent
}

func TestAgentSummary_RootCauseEvidenceAndRequestableAction(t *testing.T) {
	h := remediationHarness(t)
	s := summaryOf(t, h, inv)
	if s.Status != "concluded" || s.Reason != "one idle transaction blocks the waiters" ||
		!strings.Contains(s.RootCause, "Idle-in-transaction lock holder") ||
		!strings.Contains(s.RootCause, "pid 4242") {
		t.Fatalf("header %+v", s)
	}
	if len(s.Evidence) != 2 || s.Evidence[0].EvidenceID != string(ev1) ||
		s.Evidence[0].Numbers["blocker_pid"] != 4242 ||
		s.Evidence[0].Numbers["blocker_xact_age_s"] != 90.5 ||
		s.Evidence[1].EvidenceID != string(ev2) || s.Evidence[1].Numbers["rows"] != 2 {
		t.Fatalf("evidence: the chain's citations with their numbers, root first: %+v",
			s.Evidence)
	}
	byID := map[string]AgentAction{}
	for _, a := range s.Actions {
		byID[a.RemediationID] = a
	}
	cancel, ok := byID[cancelID]
	if !ok || !cancel.Requestable || cancel.Title != "Cancel the statement of blocking "+
		"backend pid 4242" || cancel.GateVerdict != "queue_for_approval" ||
		len(cancel.EvidenceIDs) != 1 || cancel.EvidenceIDs[0] != string(ev1) {
		t.Fatalf("cancel action %+v in %+v", cancel, s.Actions)
	}
	if len(s.Actions) != 3 {
		t.Fatalf("every candidate is listed: %+v", s.Actions)
	}
	if s.WhyNoAction != nil {
		t.Fatalf("an action can be requested, so no why-not: %+v", s.WhyNoAction)
	}
}

func TestAgentSummary_NoCandidateNamesTheCandidateLink(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.put(lockDetail())
	s := summaryOf(t, h, inv)
	if len(s.Actions) != 0 || s.Actions == nil {
		t.Fatalf("actions must be an empty list: %#v", s.Actions)
	}
	w := s.WhyNoAction
	if w == nil || w.Link != "candidate" || w.Sentence == "" || w.NoFix == "" ||
		w.Fix != nil {
		t.Fatalf("why not: %+v", w)
	}
}

func TestAgentSummary_HeldBackCandidateNamesTheGateLink(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	d := lockDetail()
	d.Investigation.Summary.Proposals = []sre.ActionProposal{{Feature: "sequence",
		Action: "manual migration", Targets: []string{"s"}, Verdict: "manual_only",
		Reason: "emergency_stop", RiskTier: "high"}}
	h.orders.put(d)
	s := summaryOf(t, h, inv)
	if len(s.Actions) != 1 || s.Actions[0].Requestable {
		t.Fatalf("actions %+v", s.Actions)
	}
	w := s.WhyNoAction
	if w == nil || w.Link != "emergency_stop" || w.Reason != "emergency_stop" ||
		w.Sentence == "" {
		t.Fatalf("the gate's reason maps to its why-not link: %+v", w)
	}
}

func TestAgentSummary_FailedInvestigationIsAFailedSummaryNot500(t *testing.T) {
	hs := newHarness(t, DefaultLimits())
	d := lockDetail()
	d.Investigation.State, d.Investigation.FailureCode = sre.StateFailed, "probe_timeout"
	d.Hypotheses, d.Investigation.Summary = nil, sre.Summary{}
	hs.orders.put(d)
	h := NewHandler(hs.svc, testAuth, HandlerOptions{Now: hs.clock.Now})
	w := call(t, h, "GET", base+"/databases/orders/investigations/"+string(inv)+"/result",
		"propose-token", "")
	var r Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("a failed investigation is a 200 result: %d %s", w.Code, w.Body.String())
	}
	s := r.SummaryForAgent
	if s == nil || s.Status != "failed" || s.Reason != "probe_timeout" || s.RootCause != "" ||
		len(s.Evidence) != 0 || s.Evidence == nil || s.WhyNoAction == nil {
		t.Fatalf("failed summary %+v", s)
	}
}

func TestAgentSummary_InProgressHasNoWhyNot(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	d := lockDetail()
	d.Investigation.State = sre.StateCollecting
	h.orders.put(d)
	s := summaryOf(t, h, inv)
	if s.Status != "in_progress" || s.WhyNoAction != nil || s.Actions == nil {
		t.Fatalf("running summary %+v", s)
	}
}

func TestAgentSummary_ZeroResultIsEmptyNotNil(t *testing.T) {
	s := agentSummary(Result{}, "")
	if s.Status != "" || s.RootCause != "" || s.Evidence == nil || s.Actions == nil ||
		s.WhyNoAction != nil {
		t.Fatalf("zero result summary %#v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil || !strings.Contains(string(raw), `"evidence":[]`) ||
		!strings.Contains(string(raw), `"actions":[]`) ||
		!strings.Contains(string(raw), `"why_no_action":null`) {
		t.Fatalf("zero summary JSON %s %v", raw, err)
	}
}

func TestAgentSummary_RequestedRemediationIsNotRequestableAgain(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.put(lockDetail())
	h.orders.proposals[inv] = []sreaction.ProposalView{cancelProposal(
		sreaction.ProposalRequested)}
	s := summaryOf(t, h, inv)
	if len(s.Actions) != 1 || s.Actions[0].Requestable || s.WhyNoAction == nil {
		t.Fatalf("a requested cancel: %+v / %+v", s.Actions, s.WhyNoAction)
	}
}

// Concurrent reads share nothing: the summary is computed per result.
func TestAgentSummary_ConcurrentReadsAgree(t *testing.T) {
	h := remediationHarness(t)
	var wg sync.WaitGroup
	got := make([]AgentSummary, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := h.svc.Result(context.Background(), proposer, "orders", string(inv))
			if err == nil && r.SummaryForAgent != nil {
				got[i] = *r.SummaryForAgent
			}
		}(i)
	}
	wg.Wait()
	for i := range got {
		if got[i].RootCause == "" || len(got[i].Actions) != len(got[0].Actions) {
			t.Fatalf("read %d: %+v", i, got[i])
		}
	}
}

func TestAgentSummary_V1ResultGoldensStayByteIdentical(t *testing.T) {
	// MapResult (and the result goldens) carry no summary: the service adds
	// it, so a v1.0 mapping is unchanged.
	if r := mapLock(t, MapOptions{KeepIdentifiers: true}); r.SummaryForAgent != nil {
		t.Fatalf("MapResult must not add the summary: %+v", r.SummaryForAgent)
	}
}
