package sre

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// The investigator's outcome and the authority rule (roadmap 2.1): the
// model may agree, conclude an inconclusive graph, contest a conclusive
// root or name an unmodeled cause. Its label is normalized against the
// graph's actual state; a model-sourced root is adopted only with the
// family's earned root authority (#111), never on self-reported
// confidence; an unmodeled cause is always advisory; an outcome no cited
// claim survives for is downgraded to inconclusive.

func hyp(node causal.NodeID, st causal.Status, conf float64) causal.Hypothesis {
	return causal.Hypothesis{Node: node, Status: st, Confidence: conf,
		Label: string(node), RefutationProbe: "lock_graph"}
}

func conclusiveLock() causal.Diagnosis {
	root := hyp(causal.IdleInTxHolder, causal.StatusRoot, 0.9)
	return causal.Diagnosis{Family: causal.FamilyLockBlocking, Conclusive: true, Root: &root,
		Contributing: []causal.Hypothesis{hyp(causal.DDLLockQueue, causal.StatusContributing,
			0.8)},
		Alternatives: []causal.Hypothesis{hyp(causal.HotRowContention,
			causal.StatusAlternative, 0.3)},
		RuledOut: []causal.Hypothesis{hyp(causal.PreparedXactHolder, causal.StatusRuledOut,
			0)}}
}

func inconclusiveLock() causal.Diagnosis {
	return causal.Diagnosis{Family: causal.FamilyLockBlocking, Reason: "wait cycle",
		Alternatives: []causal.Hypothesis{
			hyp(causal.HotRowContention, causal.StatusAlternative, 0.4),
			hyp(causal.IdleInTxHolder, causal.StatusAlternative, 0.2)},
		RuledOut: []causal.Hypothesis{hyp(causal.PreparedXactHolder, causal.StatusRuledOut,
			0)}}
}

func final(outcome, root string) finalAnswer { return finalAnswer{Outcome: outcome, Root: root} }

func TestResolveOutcome_NormalizesTheLabelAgainstTheGraph(t *testing.T) {
	conc, inc := conclusiveLock(), inconclusiveLock()
	cases := []struct {
		name string
		d    causal.Diagnosis
		f    finalAnswer
		want string
		root string
	}{
		{"agree", conc, final("agree", ""), ModelAgreed, "idle_in_tx_holder"},
		{"agree naming the root", conc, final("agree", "idle_in_tx_holder"), ModelAgreed,
			"idle_in_tx_holder"},
		{"contest naming the graph root", conc, final("contest", "idle_in_tx_holder"),
			ModelAgreed, "idle_in_tx_holder"},
		{"contest", conc, final("contest", "ddl_lock_queue"), ModelContested,
			"ddl_lock_queue"},
		{"conclude on a conclusive graph", conc, final("conclude", "hot_row_contention"),
			ModelContested, "hot_row_contention"},
		{"agree naming another root", conc, final("agree", "ddl_lock_queue"),
			ModelContested, "ddl_lock_queue"},
		{"conclude", inc, final("conclude", "hot_row_contention"), ModelConcluded,
			"hot_row_contention"},
		{"contest an inconclusive graph", inc, final("contest", "hot_row_contention"),
			ModelConcluded, "hot_row_contention"},
		{"agree with an inconclusive graph", inc, final("agree", ""), ModelInconclusive, ""},
		{"inconclusive", conc, final("inconclusive", ""), ModelInconclusive, ""},
		{"conclude without a root", inc, final("conclude", ""), ModelInconclusive, ""},
		{"unknown node", conc, final("contest", "cosmic_rays"), ModelInconclusive, ""},
		{"unknown outcome", conc, final("maybe", "ddl_lock_queue"), ModelInconclusive, ""},
	}
	for _, tc := range cases {
		r := resolveOutcome(tc.d, tc.f, 1)
		if r.outcome != tc.want || r.root != tc.root {
			t.Errorf("%s: resolved %+v, want %s %q", tc.name, r, tc.want, tc.root)
		}
	}
}

func TestResolveOutcome_UncitedOutcomesAreDowngraded(t *testing.T) {
	for _, f := range []finalAnswer{final("contest", "ddl_lock_queue"),
		{Outcome: "unmodeled", Cause: &UnmodeledCause{Label: "disk", Mechanism: "slow"}}} {
		r := resolveOutcome(conclusiveLock(), f, 0)
		if r.outcome != ModelInconclusive || r.reason != DowngradeUncited {
			t.Errorf("%+v with no surviving claim: %+v, want inconclusive (uncited)", f, r)
		}
	}
	if r := resolveOutcome(conclusiveLock(), final("agree", ""), 0); r.outcome != ModelAgreed {
		t.Fatalf("agreement needs no claim of its own: %+v", r)
	}
}

func TestResolveOutcome_UnmodeledNeedsACause(t *testing.T) {
	good := finalAnswer{Outcome: "unmodeled", Cause: &UnmodeledCause{
		Label: "storage latency", Mechanism: "fsync latency rose on the volume"}}
	if r := resolveOutcome(conclusiveLock(), good, 2); r.outcome != ModelUnmodeled ||
		r.cause == nil || r.root != "" {
		t.Fatalf("unmodeled = %+v", r)
	}
	for _, c := range []*UnmodeledCause{nil, {Label: "", Mechanism: "x"},
		{Label: "x", Mechanism: " "}, {Label: strings.Repeat("x", 300), Mechanism: "y"}} {
		r := resolveOutcome(conclusiveLock(), finalAnswer{Outcome: "unmodeled", Cause: c}, 2)
		if r.outcome != ModelInconclusive || r.reason != DowngradeInvalidCause {
			t.Errorf("cause %+v: %+v, want inconclusive (invalid cause)", c, r)
		}
	}
}

func TestApplyAuthority_ContestIsAdvisoryWithoutAGrant(t *testing.T) {
	d := conclusiveLock()
	r := resolveOutcome(d, final("contest", "ddl_lock_queue"), 1)
	out, mc, contest := applyAuthority(d, r, RootGrant{Reason: "needs 16 more overrides"})
	if out.Root.Node != causal.IdleInTxHolder || !out.Conclusive {
		t.Fatalf("root changed without authority: %+v", out.Root)
	}
	if mc.Authority != ContestAdvisory || mc.Outcome != ModelContested ||
		mc.Root != "ddl_lock_queue" || mc.GraphRoot != "idle_in_tx_holder" ||
		mc.Reason != "needs 16 more overrides" || mc.Label != ModelConclusionLabel {
		t.Fatalf("conclusion = %+v", mc)
	}
	if contest == nil || contest.Authority != ContestAdvisory ||
		contest.ModelRoot != "ddl_lock_queue" || contest.GraphRoot != "idle_in_tx_holder" {
		t.Fatalf("contest = %+v", contest)
	}
}

func TestApplyAuthority_ContestIsAdoptedWithAGrant(t *testing.T) {
	d := conclusiveLock()
	r := resolveOutcome(d, final("contest", "ddl_lock_queue"), 1)
	out, mc, contest := applyAuthority(d, r, RootGrant{Granted: true, Reason: "16/16"})
	if out.Root == nil || out.Root.Node != causal.DDLLockQueue || !out.Conclusive {
		t.Fatalf("root = %+v, want the adopted ddl_lock_queue", out.Root)
	}
	if len(out.Contributing) == 0 || out.Contributing[0].Node != causal.IdleInTxHolder {
		t.Fatalf("the graph's root is not kept as contributing: %+v", out.Contributing)
	}
	if mc.Authority != ContestAdopted || contest.Authority != ContestAdopted {
		t.Fatalf("authority = %s / %s, want adopted", mc.Authority, contest.Authority)
	}
	if d.Root.Node != causal.IdleInTxHolder {
		t.Fatal("applyAuthority modified its input diagnosis")
	}
}

func TestApplyAuthority_GrantCannotAdoptARuledOutOrForeignNode(t *testing.T) {
	d := conclusiveLock()
	for _, node := range []string{"prepared_xact_holder", "connection_leak"} {
		r := resolveOutcome(d, final("contest", node), 1)
		out, mc, _ := applyAuthority(d, r, RootGrant{Granted: true, Reason: "earned"})
		if out.Root.Node != causal.IdleInTxHolder || mc.Authority != ContestAdvisory ||
			!strings.Contains(mc.Reason, "not an open hypothesis") {
			t.Errorf("%s: root %s, conclusion %+v", node, out.Root.Node, mc)
		}
	}
}

func TestApplyAuthority_ConclusionOfAnInconclusiveGraph(t *testing.T) {
	d := inconclusiveLock()
	r := resolveOutcome(d, final("conclude", "hot_row_contention"), 1)
	out, mc, contest := applyAuthority(d, r, RootGrant{Reason: "advisory: unmeasured"})
	if out.Conclusive || out.Root != nil || mc.Authority != ContestAdvisory ||
		mc.Outcome != ModelConcluded || contest != nil {
		t.Fatalf("without a grant: diagnosis %+v conclusion %+v contest %+v", out, mc, contest)
	}
	out, mc, _ = applyAuthority(d, r, RootGrant{Granted: true, Reason: "earned"})
	if !out.Conclusive || out.Root == nil || out.Root.Node != causal.HotRowContention ||
		out.Root.Status != causal.StatusRoot || mc.Authority != ContestAdopted {
		t.Fatalf("with a grant: diagnosis %+v conclusion %+v", out, mc)
	}
	for _, h := range out.Alternatives {
		if h.Node == causal.HotRowContention {
			t.Fatal("the adopted root is still listed as an alternative")
		}
	}
}

func TestApplyAuthority_UnmodeledIsAlwaysAdvisory(t *testing.T) {
	d := conclusiveLock()
	r := resolveOutcome(d, finalAnswer{Outcome: "unmodeled", Cause: &UnmodeledCause{
		Label: "storage latency", Mechanism: "fsync latency rose"}}, 1)
	out, mc, contest := applyAuthority(d, r, RootGrant{Granted: true, Reason: "earned"})
	if out.Root.Node != causal.IdleInTxHolder || mc.Authority != ContestAdvisory ||
		mc.Cause == nil || mc.Cause.Label != "storage latency" || contest != nil ||
		mc.GraphRoot != "idle_in_tx_holder" {
		t.Fatalf("unmodeled: diagnosis root %s, conclusion %+v, contest %+v", out.Root.Node,
			mc, contest)
	}
}

func TestApplyAuthority_AgreementAndInconclusiveChangeNothing(t *testing.T) {
	d := conclusiveLock()
	for _, f := range []finalAnswer{final("agree", ""), final("inconclusive", "")} {
		r := resolveOutcome(d, f, 1)
		out, mc, contest := applyAuthority(d, r, RootGrant{Granted: true})
		if out.Root.Node != causal.IdleInTxHolder || contest != nil ||
			mc.Authority != ContestAdvisory {
			t.Errorf("%+v: diagnosis root %s conclusion %+v contest %+v", f, out.Root.Node, mc,
				contest)
		}
	}
}

func TestDecodeFinal_IgnoresSelfReportedConfidence(t *testing.T) {
	with, err := decodeFinal(json.RawMessage(`{"outcome":"contest","root":"ddl_lock_queue",` +
		`"confidence":0.999,"certainty":"absolute","claims":[]}`))
	if err != nil {
		t.Fatalf("decodeFinal: %v", err)
	}
	without, err := decodeFinal(json.RawMessage(`{"outcome":"contest",` +
		`"root":"ddl_lock_queue","claims":[]}`))
	if err != nil {
		t.Fatalf("decodeFinal: %v", err)
	}
	d := conclusiveLock()
	a, ma, _ := applyAuthority(d, resolveOutcome(d, with, 1), RootGrant{Reason: "x"})
	b, mb, _ := applyAuthority(d, resolveOutcome(d, without, 1), RootGrant{Reason: "x"})
	if a.Root.Node != b.Root.Node || *ma != *mb || ma.Authority != ContestAdvisory {
		t.Fatalf("confidence changed the outcome: %+v vs %+v", ma, mb)
	}
}

func TestDecodeFinal_RejectsWhatIsNotAnObject(t *testing.T) {
	for _, raw := range []string{``, `[]`, `"agree"`, `{"outcome":`, `{"outcome":7}`} {
		if _, err := decodeFinal(json.RawMessage(raw)); err == nil {
			t.Errorf("decodeFinal(%q) accepted", raw)
		}
	}
}

func TestAuthorityFamily_IsTheAdoptedNodesFamily(t *testing.T) {
	slo := causal.Diagnosis{Family: causal.FamilySLO}
	if f := authorityFamily(slo, "idle_in_tx_holder"); f != string(causal.FamilyLockBlocking) {
		t.Fatalf("SLO triage family for a lock node = %s, want lock_blocking", f)
	}
	if f := authorityFamily(conclusiveLock(), "ddl_lock_queue"); f != "lock_blocking" {
		t.Fatalf("family = %s", f)
	}
	if f := authorityFamily(conclusiveLock(), "cosmic_rays"); f != "lock_blocking" {
		t.Fatalf("unknown node falls back to the diagnosis family, got %s", f)
	}
}
