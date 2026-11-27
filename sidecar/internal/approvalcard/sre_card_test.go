package approvalcard

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

// A Sage SRE backend cancel is an approval card like any other queued
// action (policy-08): the SRE summary is its rationale, its target is the
// exact backend incarnation (pid and backend_start), and the card hash
// covers that incarnation, so a reused pid is another card.

const sreProposalKey = "sre_proposal:44444444-4444-4444-8444-444444444444"

const sreSummary = "orders: pid 5151 (user app) blocks 2 sessions (lock wait); identity " +
	"rechecked before the signal; mitigation only"

func sreAction(id int) store.QueuedAction {
	a := queued(id, "cancel_backend", "SELECT pg_cancel_backend(5151)", "", "moderate")
	a.IdentityKey = sreProposalKey
	return a
}

func sreFinding(pid any, start string) *FindingRow {
	detail := map[string]any{"source": "sage_sre", "summary": sreSummary,
		"proposal_id": strings.TrimPrefix(sreProposalKey, "sre_proposal:")}
	if pid != nil {
		detail["signal_target"] = map[string]any{"pid": pid, "backend_start": start}
	}
	return &FindingRow{ID: 600, Category: "sre_action", Severity: "critical",
		ObjectType: "backend", Object: sreProposalKey, Title: "Cancel blocking backend pid 5151",
		Recommendation: "Approve or deny the proposed cancel from the Cases panel, the " +
			"approval queue or ChatOps.", Detail: detail}
}

func sreCard(pid any, start string) Card {
	a := sreAction(7)
	return Assemble(Inputs{Database: "orders", Action: a, Finding: sreFinding(pid, start),
		Contract: nil, TrustLevel: "advisory", Now: now})
}

func TestAssembleSRECancelShowsTheSummaryAndTheTarget(t *testing.T) {
	c := sreCard(float64(5151), "2026-11-09T08:30:00.123456Z")
	if c.Rationale == nil || c.Rationale.Source != "rule" || c.Rationale.Text != sreSummary {
		t.Fatalf("rationale = %+v, want the SRE summary", c.Rationale)
	}
	want := "backend pid 5151 started 2026-11-09T08:30:00.123456Z"
	if len(c.Targets) != 1 || c.Targets[0] != want {
		t.Fatalf("targets = %q, want [%q]", c.Targets, want)
	}
	text := Text(c, now)
	if !strings.Contains(text, sreSummary) || !strings.Contains(text, want) {
		t.Fatalf("card text lacks the summary or the target:\n%s", text)
	}
	if strings.Contains(text, "sre_proposal:") {
		t.Fatalf("card text shows the identity key as a target:\n%s", text)
	}
	if c.ActionType != "cancel_backend" || c.SQL != "SELECT pg_cancel_backend(5151)" {
		t.Fatalf("card = %s %q", c.ActionType, c.SQL)
	}
}

func TestSRECardHashCoversTheBackendIncarnation(t *testing.T) {
	base := sreCard(float64(5151), "2026-11-09T08:30:00.123456Z")
	same := sreCard(float64(5151), "2026-11-09T08:30:00.123456Z")
	if base.CardHash == "" || base.CardHash != same.CardHash {
		t.Fatalf("hash is not deterministic: %q vs %q", base.CardHash, same.CardHash)
	}
	for name, other := range map[string]Card{
		"reused pid, new backend": sreCard(float64(5151), "2026-11-09T08:31:00Z"),
		"another pid":             sreCard(float64(5152), "2026-11-09T08:30:00.123456Z"),
		"no recorded target":      sreCard(nil, ""),
	} {
		if other.CardHash == base.CardHash {
			t.Errorf("%s: the card hash did not change", name)
		}
	}
	if base.CardHash == ContentHash(sreAction(7)) {
		t.Fatal("a backend-signal card hash ignores its target")
	}
}

// The target only binds backend signals: any other card's hash is the
// queue row's content hash, whatever its finding says.
func TestNonSignalCardHashIgnoresSignalTarget(t *testing.T) {
	a := queued(8, "analyze_table", "ANALYZE public.orders", "", "safe")
	f := sreFinding(float64(5151), "2026-11-09T08:30:00Z")
	f.Category = "stale_statistics"
	c := Assemble(Inputs{Database: "orders", Action: a, Finding: f, Now: now})
	if c.CardHash != ContentHash(a) {
		t.Fatalf("non-signal hash = %q, want ContentHash %q", c.CardHash, ContentHash(a))
	}
}

func TestBackendSignal(t *testing.T) {
	for typ, want := range map[string]bool{"cancel_backend": true,
		"terminate_backend": true, "": false, "analyze_table": false,
		"Cancel_Backend": false, "cancel_backend_x": false} {
		if got := BackendSignal(typ); got != want {
			t.Errorf("BackendSignal(%q) = %v, want %v", typ, got, want)
		}
	}
	if BackendSignalCardTTL <= 0 || BackendSignalCardTTL > DefaultCardTTL ||
		BackendSignalCardTTL.Minutes() != 15 {
		t.Fatalf("backend-signal card TTL = %v, want 15m", BackendSignalCardTTL)
	}
}

// A malformed signal target (a string pid, a missing start) still hashes
// deterministically and never matches a well-formed one.
func TestSRECardHashMalformedTarget(t *testing.T) {
	good := sreCard(float64(5151), "2026-11-09T08:30:00Z")
	for name, c := range map[string][2]Card{
		"string pid": {sreCard("5151", "2026-11-09T08:30:00Z"),
			sreCard("5151", "2026-11-09T08:30:00Z")},
		"empty start": {sreCard(float64(5151), ""), sreCard(float64(5151), "")},
	} {
		if c[0].CardHash != c[1].CardHash {
			t.Errorf("%s: hash not deterministic", name)
		}
		if c[0].CardHash == good.CardHash {
			t.Errorf("%s: hash equals the well-formed target's", name)
		}
	}
}

// No concurrent access tests: Assemble and the hash are pure functions of
// their inputs and share no state.

// A queue row with no action_type (written before the column existed, or
// by a store without metadata) is still a backend signal when its SQL
// signals a backend, so its card keeps the 15-minute TTL, the missing
// Snooze and the target-bound hash.
func TestUntypedSignalSQLIsABackendSignal(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT pg_cancel_backend(5151)":              "cancel_backend",
		"select pg_catalog.pg_terminate_backend(77);": "terminate_backend",
		"ANALYZE public.orders":                       "analyze_table",
		"SELECT 1":                                    "",
	} {
		a := queued(9, "", sql, "", "moderate")
		signal := BackendSignal(want)
		if got := SignalsBackend(a); got != signal {
			t.Errorf("%q: SignalsBackend = %v, want %v", sql, got, signal)
		}
		c := Assemble(Inputs{Database: "orders", Action: a, Now: now})
		if c.ActionType != want {
			t.Errorf("%q: card action type %q, want %q", sql, c.ActionType, want)
		}
		ref := CardRefOf(c)
		if ref.NoSnooze != signal || (ref.MaxTTL == BackendSignalCardTTL) != signal {
			t.Errorf("%q: card ref %+v, backend signal %v", sql, ref, signal)
		}
		if (c.CardHash != ContentHash(a)) != signal {
			t.Errorf("%q: card hash bound to a target = %v, want %v", sql,
				c.CardHash != ContentHash(a), signal)
		}
	}
	if SignalsBackend(queued(9, "analyze_table", "SELECT pg_cancel_backend(1)", "",
		"safe")) {
		t.Fatal("a typed row was reclassified by its SQL")
	}
}
