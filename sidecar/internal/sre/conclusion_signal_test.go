package sre

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// sre-08: the conclusion signal is the only thing an investigation hands
// the notify stack. It is built for final diagnoses of an incident only,
// and its reason is one redacted line of at most 200 runes.
//
// No concurrent-access tests here: buildConclusionSignal is a pure
// function of its arguments (the notifier's races are in the DB test).

func lockCandidate() ConclusionCandidate {
	return ConclusionCandidate{InvestigationID: "11111111-2222-4333-8444-555555555555",
		IncidentID: "6f0d3c1e-8d0a-4c1b-9a51-0b6d1f6e2a11", CaseID: "incident:orders:lock",
		Kind: TriggerLock, State: StateConcluded, Family: "lock_blocking",
		Root: "idle_in_tx_holder", RootLabel: "idle-in-transaction holder",
		Reason:      "root blocker pid 7 has been idle in transaction for 9m",
		ConcludedAt: time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)}
}

func trackedOrders() TrackedIncident {
	return TrackedIncident{Database: "orders", Severity: "critical"}
}

func TestConclusionSignal_ConcludedCarriesTheDiagnosis(t *testing.T) {
	c := lockCandidate()
	sig, ok := buildConclusionSignal(c, trackedOrders())
	if !ok {
		t.Fatal("a concluded incident investigation built no signal")
	}
	want := ConclusionSignal{InvestigationID: c.InvestigationID, IncidentID: c.IncidentID,
		CaseID: c.CaseID, Database: "orders", Family: "lock_blocking",
		State: StateConcluded, RootLabel: "idle-in-transaction holder",
		Reason: c.Reason, Severity: "critical", ConcludedAt: c.ConcludedAt}
	if sig != want {
		t.Fatalf("signal = %+v\nwant     %+v", sig, want)
	}
}

func TestConclusionSignal_InconclusiveHasNoRoot(t *testing.T) {
	c := lockCandidate()
	c.State, c.Root, c.RootLabel = StateInconclusive, "", ""
	c.Reason = "lock graph empty when probed"
	sig, ok := buildConclusionSignal(c, trackedOrders())
	if !ok || sig.State != StateInconclusive || sig.RootLabel != "" ||
		sig.Reason != "lock graph empty when probed" {
		t.Fatalf("inconclusive signal = %+v (%v)", sig, ok)
	}
}

func TestConclusionSignal_ConcludedWithoutLabelFallsBackToRootNode(t *testing.T) {
	c := lockCandidate()
	c.RootLabel = ""
	sig, ok := buildConclusionSignal(c, trackedOrders())
	if !ok || sig.RootLabel != "idle_in_tx_holder" {
		t.Fatalf("root label = %q (%v), want the root node id", sig.RootLabel, ok)
	}
}

func TestConclusionSignal_FamilyFallsBackToTriggerKind(t *testing.T) {
	c := lockCandidate()
	c.Family = ""
	sig, ok := buildConclusionSignal(c, trackedOrders())
	if !ok || sig.Family != string(TriggerLock) {
		t.Fatalf("family = %q (%v), want %q", sig.Family, ok, TriggerLock)
	}
}

func TestConclusionSignal_OnlyFinalDiagnosesBuildASignal(t *testing.T) {
	for _, st := range []State{StateFailed, StateExpired, StateCancelled, StatePaused,
		StateQueued, StateCollecting, StateEvaluating, StateNeedsEvidence,
		State("superseded_by_v9"), State("")} {
		c := lockCandidate()
		c.State = st
		if sig, ok := buildConclusionSignal(c, trackedOrders()); ok {
			t.Errorf("state %q built a signal: %+v", st, sig)
		}
	}
}

func TestConclusionSignal_NoIncidentNoSignal(t *testing.T) {
	c := lockCandidate()
	c.IncidentID = ""
	if sig, ok := buildConclusionSignal(c, trackedOrders()); ok {
		t.Fatalf("an investigation without an incident built %+v", sig)
	}
	c.IncidentID = "   "
	if _, ok := buildConclusionSignal(c, trackedOrders()); ok {
		t.Fatal("a blank incident id built a signal")
	}
}

func TestConclusionSignal_ZeroValuesBuildNothing(t *testing.T) {
	if sig, ok := buildConclusionSignal(ConclusionCandidate{}, TrackedIncident{}); ok {
		t.Fatalf("zero candidate built %+v", sig)
	}
	c := lockCandidate()
	c.InvestigationID = ""
	if _, ok := buildConclusionSignal(c, trackedOrders()); ok {
		t.Fatal("a candidate without an investigation id built a signal")
	}
}

func TestConclusionSignal_UnknownSeverityIsWarning(t *testing.T) {
	sig, ok := buildConclusionSignal(lockCandidate(),
		TrackedIncident{Database: "orders", Severity: "apocalyptic"})
	if !ok || sig.Severity != "warning" {
		t.Fatalf("severity = %q (%v), want warning", sig.Severity, ok)
	}
	sig, _ = buildConclusionSignal(lockCandidate(), TrackedIncident{Database: "orders"})
	if sig.Severity != "warning" {
		t.Fatalf("empty severity = %q, want warning", sig.Severity)
	}
}

func TestConclusionSignal_ReasonClippedAtRuneBoundary(t *testing.T) {
	c := lockCandidate()
	c.Reason = strings.Repeat("é", 200) + "Ж" // 201 runes, 2 bytes each
	sig, ok := buildConclusionSignal(c, trackedOrders())
	if !ok {
		t.Fatal("no signal")
	}
	if n := utf8.RuneCountInString(sig.Reason); n != 200 {
		t.Fatalf("reason has %d runes, want 200", n)
	}
	if !utf8.ValidString(sig.Reason) || strings.Contains(sig.Reason, "Ж") {
		t.Fatalf("reason was not clipped at a rune boundary: %q", sig.Reason)
	}
	c.Reason = strings.Repeat("ü", 200) // exactly 200 runes stays whole
	if sig, _ = buildConclusionSignal(c, trackedOrders()); sig.Reason != c.Reason {
		t.Fatalf("a 200-rune reason changed: %d runes", utf8.RuneCountInString(sig.Reason))
	}
}

func TestConclusionSignal_ReasonIsOneRedactedLine(t *testing.T) {
	c := lockCandidate()
	c.Reason = "blocker ran UPDATE t SET v = 'hunter2-literal'\nwhere\tpassword=s3cr3t-pw\r\n" +
		"from postgres://admin:pw@db.internal/orders"
	sig, ok := buildConclusionSignal(c, trackedOrders())
	if !ok {
		t.Fatal("no signal")
	}
	for _, leak := range []string{"hunter2-literal", "s3cr3t-pw", "admin:pw", "\n", "\r", "\t"} {
		if strings.Contains(sig.Reason, leak) {
			t.Errorf("reason leaks %q: %q", leak, sig.Reason)
		}
	}
	if !strings.Contains(sig.Reason, "[redacted") {
		t.Errorf("reason was not redacted: %q", sig.Reason)
	}
}

func TestConclusionSignal_RootLabelIsRedactedAndBounded(t *testing.T) {
	c := lockCandidate()
	c.RootLabel = "holder token=abc123def " + strings.Repeat("x", 400)
	sig, ok := buildConclusionSignal(c, trackedOrders())
	if !ok || strings.Contains(sig.RootLabel, "abc123def") ||
		utf8.RuneCountInString(sig.RootLabel) > 200 {
		t.Fatalf("root label = %q (%v)", sig.RootLabel, ok)
	}
}
