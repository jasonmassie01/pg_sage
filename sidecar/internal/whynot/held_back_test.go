package whynot

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-08b: a "Held back" row names the chain link its gate reason fails,
// with the chain's own sentence, so the row and the panel agree.
// No concurrency test: HeldBackLink reads an immutable table.

func TestHeldBackLinkNamesTheFailingLink(t *testing.T) {
	for _, c := range []struct {
		reason   string
		link     LinkName
		sentence string
	}{
		{"approval_required", LinkPolicy, "Asks first: approval required."},
		{"outside_maintenance_window", LinkWindow, "Outside the maintenance window."},
		{"rate_limit_exceeded", LinkBudget, "The change budget for this window is spent."},
		{"replica_mutation", LinkPrivilege,
			"This database is a read-only replica, so pg_sage changes nothing on it."},
		{"observe_only", LinkTrust,
			"Trust level is observation: pg_sage watches and runs nothing."},
		{"awaiting_verification", LinkVerification,
			"An earlier change on this object is still being verified."},
		{"emergency_stop", LinkEmergencyStop, "Emergency stop is on."},
	} {
		link, sentence, ok := HeldBackLink(c.reason)
		if !ok || link != c.link || sentence != c.sentence {
			t.Errorf("HeldBackLink(%q) = %q, %q, %v; want %q, %q", c.reason, link,
				sentence, ok, c.link, c.sentence)
		}
	}
}

// A reason that lets the change run fails no link; an unknown, empty or
// sentence-shaped reason has no link, so the row keeps its own words.
func TestHeldBackLinkRefusesReasonsThatFailNoLink(t *testing.T) {
	for _, reason := range []string{"authorized", "operator_approved", "autonomy_l3",
		"deadline_override", "", "new_gate_reason", "approval_required ",
		"live DDL preflight has warnings; review required"} {
		if link, sentence, ok := HeldBackLink(reason); ok || link != "" || sentence != "" {
			t.Errorf("HeldBackLink(%q) = %q, %q, %v; want no link", reason, link,
				sentence, ok)
		}
	}
}

// Boundary: every failing ledger reason names one of the ten links.
func TestHeldBackLinkCoversEveryFailingLedgerReason(t *testing.T) {
	known := map[LinkName]bool{}
	for _, name := range Links() {
		known[name] = true
	}
	failing := 0
	for _, reason := range policy.LedgerReasons() {
		m, _ := mappingFor(string(reason))
		link, sentence, ok := HeldBackLink(string(reason))
		if m.allows {
			if ok {
				t.Errorf("%s allows the change but names link %q", reason, link)
			}
			continue
		}
		failing++
		if !ok || !known[link] || sentence == "" {
			t.Errorf("%s = %q, %q, %v; want one of the ten links", reason, link,
				sentence, ok)
		}
	}
	if failing < 40 {
		t.Fatalf("only %d failing reasons checked; the ledger has more", failing)
	}
}
