package sre

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The tool-calling investigator in a real investigation: real store,
// scripted probes, a fake OpenAI-compatible model playing a transcript.
// Every probe call is stored evidence with its digest; the conclusion
// cites stored evidence; model roots follow the authority rule.

// lastAliasOf finds the last alias the prompt gave a probe result.
func lastAliasOf(t *testing.T, body string, probe probes.ID, status string) string {
	t.Helper()
	re := regexp.MustCompile(`(E\d+) \[` + regexp.QuoteMeta(string(probe)) + " " +
		regexp.QuoteMeta(status) + `\]`)
	all := re.FindAllStringSubmatch(body, -1)
	if len(all) == 0 {
		t.Errorf("prompt has no %s %s evidence: %s", probe, status, body)
		return "E0"
	}
	return all[len(all)-1][1]
}

func agreeingTranscript(t *testing.T) *fakeModel {
	return newFakeModel(t,
		call(ToolGraphState, `{}`),
		call(ToolRunProbe, probeArgs(probes.LongTransactions)),
		submit(func(body string) invFinal {
			return invFinal{Outcome: "agree", Root: "idle_in_tx_holder", Claims: []invClaim{
				idleClaim(aliasOf(t, body, probes.LockGraph, "ok")),
				{Text: "pid 4242 has kept its transaction open for 95 s.",
					EvidenceIDs: []string{lastAliasOf(t, body, probes.LongTransactions, "ok")}}}}
		}))
}

func TestInvestigator_AgreesAndCitesToolEvidence(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := agreeingTranscript(t)
	c, _ := investigatorCoordinator(t, ctx, st, longTxRunner(), m.client(), invOptions{})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-agree"))
	assertIdleRoot(t, st, inv)
	run := transcriptOf(t, inv)
	if run.Plan != PlanNarrow || run.Stop != "final" || run.ModelCalls != 3 ||
		run.Label != InvestigatorRunLabel {
		t.Fatalf("run = %+v, want the narrow plan concluding after 3 model calls", run)
	}
	stored := evidenceByID(t, st, inv)
	probesRun := stepsOf(run, ToolRunProbe)
	if len(probesRun) != 1 || probesRun[0].EvidenceID == "" {
		t.Fatalf("run_probe steps = %+v", probesRun)
	}
	ev, ok := stored[probesRun[0].EvidenceID]
	if !ok || ev.ProbeID != string(probes.LongTransactions) ||
		probesRun[0].Digest != digestOf(ev) || !ev.VerifyHash() {
		t.Fatalf("run_probe evidence %s = %+v (ok %v), digest %s", probesRun[0].EvidenceID,
			ev, ok, probesRun[0].Digest)
	}
	if g := stepsOf(run, ToolGraphState); len(g) != 1 || g[0].EvidenceID != "" {
		t.Fatalf("graph_state steps = %+v, want one, not evidence", g)
	}
	n := inv.Summary.Narrative
	if n == nil || len(n.Claims) != 2 {
		t.Fatalf("narrative = %+v, want 2 cited claims", n)
	}
	for _, cl := range n.Claims {
		for _, id := range cl.EvidenceIDs {
			if _, ok := stored[id]; !ok {
				t.Fatalf("claim %q cites %s, not evidence of this investigation", cl.Text, id)
			}
		}
	}
	if n.Claims[1].EvidenceIDs[0] != probesRun[0].EvidenceID {
		t.Fatalf("second claim cites %s, want the run_probe evidence %s",
			n.Claims[1].EvidenceIDs[0], probesRun[0].EvidenceID)
	}
	mc := inv.Summary.ModelConclusion
	if mc == nil || mc.Outcome != ModelAgreed || mc.Authority != ContestAdvisory {
		t.Fatalf("model conclusion = %+v", mc)
	}
	if inv.ModelTurns != 0 || inv.Summary.ModelRanking != nil {
		t.Fatalf("the investigator used the review turn: turns %d ranking %+v",
			inv.ModelTurns, inv.Summary.ModelRanking)
	}
	if got := reservationsOf(t, st, inv); got[InvestigatorCallerKind] != 3 ||
		got["sre_investigation"] != 0 {
		t.Fatalf("reservations = %v, want 3 investigator calls", got)
	}
	ps := payloads(t, st, inv, EventModelReviewed)
	if len(ps) != 1 || ps[0]["investigator"] != PlanNarrow || ps[0]["outcome"] != ModelAgreed {
		t.Fatalf("model_reviewed events = %+v", ps)
	}
}

func TestInvestigator_PromptCarriesTheGraphTheEvidenceAndTheBudget(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := agreeingTranscript(t)
	c, _ := investigatorCoordinator(t, ctx, st, longTxRunner(), m.client(), invOptions{})
	startAndRun(t, ctx, c, lockTrigger("inv-prompt"))
	first := m.body(t, 0)
	if !containsAll(first, "idle_in_tx_holder", "conclusive", "E1 [lock_graph ok]",
		ToolRunProbe, ToolSubmit, "unmodeled", `<data label=\"evidence`) {
		t.Fatalf("first prompt misses the graph, evidence or tools: %s", first)
	}
	narrow := DefaultInvestigatorPlans()[PlanNarrow]
	if !strings.Contains(first, itoa(int64(narrow.MaxSteps))+" model steps") {
		t.Fatalf("first prompt does not state the step budget: %s", first)
	}
	if strings.Contains(first, `"name":"`+ToolExplain+`"`) {
		t.Fatal("the narrow plan offered EXPLAIN without an explainer")
	}
}

func TestInvestigator_OperatorStartRunsTheBroadTriage(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, submitFixed(invFinal{Outcome: "inconclusive"}))
	runner := idleChainRunner()
	c, _ := investigatorCoordinator(t, ctx, st, runner, m.client(), invOptions{})
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "operator:checkout", Kind: TriggerOperator,
		Subject: "checkout is slow", Actor: "user:1"})
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" {
		t.Fatalf("operator triage = %s root %q (%s), want the idle holder found",
			inv.State, inv.Summary.Root, inv.FailureCode)
	}
	run := transcriptOf(t, inv)
	broad := DefaultInvestigatorPlans()[PlanBroad]
	if run.Plan != PlanBroad || run.Budget.MaxSteps != broad.MaxSteps {
		t.Fatalf("run plan %s budget %+v, want the broad plan", run.Plan, run.Budget)
	}
	seen := map[string]bool{}
	for _, e := range evidenceByID(t, st, inv) {
		seen[e.ProbeID] = true
	}
	for _, id := range []probes.ID{probes.LockGraph, probes.ConnectionSaturation,
		probes.PlanRegressions, probes.SageActions} {
		if !seen[string(id)] {
			t.Errorf("broad triage did not collect %s (collected %v)", id, seen)
		}
	}
}

func TestInvestigator_ContestStaysAdvisoryWithoutAuthority(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Reason: "needs 16 more correct overrides"}}
	m := newFakeModel(t, submit(func(body string) invFinal {
		return invFinal{Outcome: "contest", Root: "ddl_lock_queue", Claims: []invClaim{
			idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}
	}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
		invOptions{auth: auth})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-contest"))
	assertIdleRoot(t, st, inv)
	mc, contest := inv.Summary.ModelConclusion, inv.Summary.ModelContest
	if mc == nil || mc.Outcome != ModelContested || mc.Authority != ContestAdvisory ||
		contest == nil || contest.Authority != ContestAdvisory ||
		contest.ModelRoot != "ddl_lock_queue" {
		t.Fatalf("conclusion %+v contest %+v", mc, contest)
	}
	if fams := auth.families(); len(fams) != 1 || fams[0] != "lock_blocking" {
		t.Fatalf("authority asked for %v, want lock_blocking once", fams)
	}
	ps := payloads(t, st, inv, EventModelDisagreed)
	if len(ps) != 1 || ps[0]["authority"] != ContestAdvisory ||
		ps[0]["model_root"] != "ddl_lock_queue" || ps[0]["graph_root"] != "idle_in_tx_holder" {
		t.Fatalf("model_disagreed = %+v", ps)
	}
}

func TestInvestigator_ContestIsAdoptedWithEarnedAuthority(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Granted: true, Reason: "16/16 held out"}}
	m := newFakeModel(t, submit(func(body string) invFinal {
		return invFinal{Outcome: "contest", Root: "ddl_lock_queue", Claims: []invClaim{
			idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}
	}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
		invOptions{auth: auth})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-adopt"))
	if inv.State != StateConcluded || inv.Summary.Root != "ddl_lock_queue" {
		t.Fatalf("investigation %s root %q, want the adopted ddl_lock_queue", inv.State,
			inv.Summary.Root)
	}
	if s := statusesOf(t, st, inv); s["idle_in_tx_holder"] != HypothesisContributing {
		t.Fatalf("the graph's root is not kept as contributing: %v", s)
	}
	if mc := inv.Summary.ModelConclusion; mc == nil || mc.Authority != ContestAdopted {
		t.Fatalf("model conclusion = %+v", mc)
	}
}

func TestInvestigator_ConcludesAnInconclusiveGraphOnlyWithAuthority(t *testing.T) {
	for _, granted := range []bool{false, true} {
		st, _, ctx := liveStore(t, budgetLimits())
		auth := &fakeAuthority{grant: RootGrant{Granted: granted, Reason: "rule says so"}}
		var named string
		m := newFakeModel(t, submit(func(body string) invFinal {
			named = openUnproven(t, body)
			return invFinal{Outcome: "conclude", Root: named, Claims: []invClaim{{
				Text: "The waits form a cycle.", EvidenceIDs: []string{
					aliasOf(t, body, probes.LockGraph, "ok")}}}}
		}))
		c, _ := investigatorCoordinator(t, ctx, st, cycleRunner(), m.client(),
			invOptions{auth: auth})
		inv := startAndRun(t, ctx, c, lockTrigger("inv-conclude-"+strconv.FormatBool(granted)))
		mc := inv.Summary.ModelConclusion
		if mc == nil || mc.Outcome != ModelConcluded || mc.Root != named {
			t.Fatalf("granted=%v: model conclusion %+v (named %s)", granted, mc, named)
		}
		switch {
		case granted && (inv.State != StateConcluded || inv.Summary.Root != named ||
			mc.Authority != ContestAdopted):
			t.Fatalf("granted: %s root %q authority %s", inv.State, inv.Summary.Root,
				mc.Authority)
		case !granted && (inv.State != StateInconclusive || inv.Summary.Root != "" ||
			mc.Authority != ContestAdvisory):
			t.Fatalf("not granted: %s root %q authority %s", inv.State, inv.Summary.Root,
				mc.Authority)
		}
	}
}

// cycleRunner scripts a lock wait cycle: the graph has no root.
func cycleRunner() *scriptedRunner {
	edge := func(waiter, blocker int64) probes.Row {
		return probes.Row{"waiter_pid": waiter, "lock_type": "transactionid",
			"requested_mode": "ShareLock", "relation": nil, "blocker_pid": blocker,
			"blocker_kind": "backend", "blocker_state": "active", "blocker_waiting": true,
			"blocker_xact_age_s": 1.0}
	}
	return newScriptedRunner().
		script(probes.LockGraph, rows(probes.LockGraph, edge(10, 20), edge(20, 10))).
		script(probes.PreparedXacts, rows(probes.PreparedXacts))
}

func TestInvestigator_UnmodeledCauseIsAlwaysAdvisory(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Granted: true, Reason: "earned"}}
	m := newFakeModel(t, submit(func(body string) invFinal {
		return invFinal{Outcome: "unmodeled", Cause: &UnmodeledCause{
			Label:     "application deploy holding locks",
			Mechanism: "a deploy job opened a transaction and paused"},
			Claims: []invClaim{idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}
	}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
		invOptions{auth: auth})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-unmodeled"))
	assertIdleRoot(t, st, inv)
	mc := inv.Summary.ModelConclusion
	if mc == nil || mc.Outcome != ModelUnmodeled || mc.Authority != ContestAdvisory ||
		mc.Cause == nil || mc.Cause.Label != "application deploy holding locks" {
		t.Fatalf("model conclusion = %+v", mc)
	}
	if len(auth.families()) != 0 {
		t.Fatalf("authority asked for an unmodeled cause: %v", auth.families())
	}
}

func TestInvestigator_HallucinatedEvidenceDowngradesTheOutcome(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Granted: true, Reason: "earned"}}
	m := newFakeModel(t, submitFixed(invFinal{Outcome: "contest", Root: "ddl_lock_queue",
		Claims: []invClaim{
			{Text: "The DDL waited 300 s.", EvidenceIDs: []string{"E97"}},
			{Text: "A migration is stuck.", EvidenceIDs: nil}}}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
		invOptions{auth: auth})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-hallucinated"))
	assertIdleRoot(t, st, inv)
	run := transcriptOf(t, inv)
	if run.DroppedClaims["unknown_evidence"] != 1 || run.DroppedClaims["uncited"] != 1 {
		t.Fatalf("dropped claims = %v", run.DroppedClaims)
	}
	mc := inv.Summary.ModelConclusion
	if mc == nil || mc.Outcome != ModelInconclusive || inv.Summary.ModelContest != nil ||
		inv.Summary.Narrative != nil {
		t.Fatalf("conclusion %+v contest %+v narrative %+v", mc, inv.Summary.ModelContest,
			inv.Summary.Narrative)
	}
	if len(auth.families()) != 0 {
		t.Fatalf("authority asked for an uncited contest: %v", auth.families())
	}
}

func TestInvestigator_InvalidFinalOutcomeIsInconclusive(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, submit(func(body string) invFinal {
		return invFinal{Outcome: "probably", Root: "ddl_lock_queue", Claims: []invClaim{
			idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}
	}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(), invOptions{})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-invalid-final"))
	assertIdleRoot(t, st, inv)
	if mc := inv.Summary.ModelConclusion; mc == nil || mc.Outcome != ModelInconclusive {
		t.Fatalf("model conclusion = %+v", mc)
	}
}
