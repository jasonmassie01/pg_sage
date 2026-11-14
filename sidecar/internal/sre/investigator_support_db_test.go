package sre

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The Support-evidence rule in a real investigation (sre-03 Phase 2):
// with InvestigatorConfig.RequireSupportCitation on, a contest whose
// claims cite nothing the graph records as support for the model's root
// is inconclusive (unsupported_root), even under earned authority; a
// contest whose claim cites that support is applied as before. With the
// flag off the Phase 1 rule stands.

func contestOn(t *testing.T, root string, require bool, subject string) (Investigation,
	*PostgresStore, *fakeAuthority) {
	t.Helper()
	st, _, ctx := liveStore(t, budgetLimits())
	auth := &fakeAuthority{grant: RootGrant{Granted: true, Reason: "16/16 held out"}}
	m := newFakeModel(t, submit(func(body string) invFinal {
		return invFinal{Outcome: "contest", Root: root, Claims: []invClaim{
			idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}
	}))
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
		invOptions{auth: auth, config: InvestigatorConfig{RequireSupportCitation: require}})
	return startAndRun(t, ctx, c, lockTrigger(subject)), st, auth
}

// lock_graph shows pid 20 queued for an AccessExclusiveLock behind the
// idle root: it is support of ddl_lock_queue, so the contest stands.
func TestInvestigatorSupport_ContestCitingTheRootsSupportIsAdopted(t *testing.T) {
	inv, st, _ := contestOn(t, "ddl_lock_queue", true, "inv-support-kept")
	if inv.State != StateConcluded || inv.Summary.Root != "ddl_lock_queue" {
		t.Fatalf("investigation %s root %q, want the adopted ddl_lock_queue", inv.State,
			inv.Summary.Root)
	}
	if mc := inv.Summary.ModelConclusion; mc == nil || mc.Outcome != ModelContested ||
		mc.Authority != ContestAdopted {
		t.Fatalf("model conclusion = %+v, want an adopted contest", mc)
	}
	if s := statusesOf(t, st, inv); s["idle_in_tx_holder"] != HypothesisContributing {
		t.Fatalf("the graph's root is not kept as contributing: %v", s)
	}
}

// No relation-lock row is support of hot_row_contention: the contest is
// inconclusive, the graph's root stands and authority is never asked.
func TestInvestigatorSupport_UnsupportedContestIsInconclusive(t *testing.T) {
	inv, st, auth := contestOn(t, "hot_row_contention", true, "inv-support-dropped")
	assertIdleRoot(t, st, inv)
	mc := inv.Summary.ModelConclusion
	if mc == nil || mc.Outcome != ModelInconclusive || mc.Root != "" ||
		mc.Authority != ContestAdvisory || !strings.Contains(mc.Reason, DowngradeUnsupported) {
		t.Fatalf("model conclusion = %+v, want inconclusive (%s)", mc, DowngradeUnsupported)
	}
	if inv.Summary.ModelContest != nil {
		t.Fatalf("an unsupported contest was stored: %+v", inv.Summary.ModelContest)
	}
	if inv.Summary.Narrative == nil || len(inv.Summary.Narrative.Claims) != 1 {
		t.Fatalf("the grounded claim must still be narrated: %+v", inv.Summary.Narrative)
	}
	if fams := auth.families(); len(fams) != 0 {
		t.Fatalf("authority asked for an unsupported contest: %v", fams)
	}
}

// Regression lock (CHECK-18): with the flag off the same contest keeps
// the Phase 1 outcome.
func TestInvestigatorSupport_FlagOffKeepsThePhaseOneContest(t *testing.T) {
	inv, st, auth := contestOn(t, "hot_row_contention", false, "inv-support-off")
	assertIdleRoot(t, st, inv)
	mc := inv.Summary.ModelConclusion
	if mc == nil || mc.Outcome != ModelContested || mc.Root != "hot_row_contention" {
		t.Fatalf("flag off: model conclusion = %+v, want the Phase 1 contest", mc)
	}
	if fams := auth.families(); len(fams) != 1 || fams[0] != "lock_blocking" {
		t.Fatalf("flag off: authority asked for %v, want lock_blocking once", fams)
	}
}
