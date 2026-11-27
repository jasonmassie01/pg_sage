package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/rca"
)

// storedChain round-trips RCA chain links the way sage.incidents stores
// them (jsonb) and incidentRow.toMap decodes them (json.Unmarshal into any).
func storedChain(t *testing.T, links []rca.ChainLink) any {
	t.Helper()
	raw, err := json.Marshal(links)
	if err != nil {
		t.Fatalf("marshal chain: %v", err)
	}
	var chain any
	if err := json.Unmarshal(raw, &chain); err != nil {
		t.Fatalf("unmarshal chain: %v", err)
	}
	return chain
}

func rcaBlocker() *rca.BlockerIdentity {
	return &rca.BlockerIdentity{
		PID:          4242,
		BackendStart: time.Date(2026, 11, 6, 9, 15, 30, 123456789, time.UTC),
		QueryStart:   time.Date(2026, 11, 6, 9, 40, 1, 654321000, time.UTC),
		QueryID:      -9001234567890123456, QuerySHA: "deadbeefcafef00d",
		State: "active", TotalBlocked: 3, ChainDepth: 2,
		LockedRelation: "public.orders", LockMode: "AccessExclusiveLock",
	}
}

// playbooks-01: the structured blocker RCA stores survives the API read.
// query_id is not carried: jsonb decoded into any turns it into a float64,
// which cannot hold every 64-bit id, and the guarded script does not use it.
func TestIncidentChainFromAnyKeepsBlocker(t *testing.T) {
	b := rcaBlocker()
	chain := storedChain(t, []rca.ChainLink{
		{Order: 1, Signal: "lock_contention", Description: "summary", Evidence: "1 chain"},
		{Order: 2, Signal: "lock_contention", Description: "root", Evidence: "pid=4242",
			Blocker: b},
	})

	got := incidentChainFromAny(chain)

	if len(got) != 2 || got[0].Blocker != nil {
		t.Fatalf("chain = %+v, want 2 links, first without blocker", got)
	}
	gb := got[1].Blocker
	if gb == nil {
		t.Fatal("blocker dropped from the second link")
	}
	if gb.PID != b.PID || !gb.BackendStart.Equal(b.BackendStart) ||
		!gb.QueryStart.Equal(b.QueryStart) ||
		gb.QuerySHA != b.QuerySHA || gb.State != b.State ||
		gb.TotalBlocked != b.TotalBlocked || gb.LockedRelation != b.LockedRelation {
		t.Fatalf("blocker = %+v, want %+v", gb, b)
	}
	if got[1].Order != 2 || got[1].Evidence != "pid=4242" {
		t.Fatalf("link fields lost: %+v", got[1])
	}
}

// Invalid input: a blocker with unparseable times or a wrong shape keeps
// the link but yields a blocker the playbook will refuse (zero times), or
// none at all; it never panics.
func TestIncidentChainFromAnyMalformedBlocker(t *testing.T) {
	chain := []any{
		map[string]any{"order": float64(2), "signal": "lock_contention",
			"blocker": map[string]any{"pid": float64(7), "backend_start": "yesterday",
				"query_start": float64(3), "query_sha": "deadbeefcafef00d",
				"state": "active"}},
		map[string]any{"order": float64(3), "signal": "lock_contention",
			"blocker": "pid 7"},
		map[string]any{"order": float64(4), "signal": "lock_contention", "blocker": nil},
		"not a link",
	}

	got := incidentChainFromAny(chain)

	if len(got) != 3 {
		t.Fatalf("links = %d, want 3 (the non-map item skipped)", len(got))
	}
	if b := got[0].Blocker; b == nil || b.PID != 7 ||
		!b.BackendStart.IsZero() || !b.QueryStart.IsZero() {
		t.Fatalf("malformed-time blocker = %+v, want pid 7 with zero times", b)
	}
	if got[1].Blocker != nil || got[2].Blocker != nil {
		t.Fatalf("non-object blockers = %+v / %+v, want nil", got[1].Blocker, got[2].Blocker)
	}
	projected := cases.ProjectIncident(cases.SourceIncident{ID: "i", DatabaseName: "prod",
		Severity: cases.SeverityCritical, RootCause: "root blocker pid 7",
		SignalIDs: []string{"lock_contention"}, CausalChain: got})
	for _, c := range projected.ActionCandidates {
		if c.ProposedSQL != "" && strings.Contains(c.ProposedSQL, "_backend(") {
			t.Fatalf("malformed blocker produced a signal script: %s", c.ProposedSQL)
		}
	}
}

// End to end through the API row mapping: an RCA lock incident row gives
// a guarded cancel script, and an old row without blocker a blocked one.
func TestSourceIncidentFromMapGuardedLockCandidate(t *testing.T) {
	row := map[string]any{
		"id": "inc-lock", "database_name": "prod", "severity": "warning",
		"root_cause": "Lock chain: root blocker pid 4242 (active) blocks 3 sessions",
		"signal_ids": []any{"lock_contention"}, "source": "deterministic",
		"confidence": 0.85, "last_detected_at": time.Now().UTC(),
		"causal_chain": storedChain(t, []rca.ChainLink{{Order: 2,
			Signal: "lock_contention", Description: "root", Blocker: rcaBlocker()}}),
	}

	cancel := cases.ProjectIncident(sourceIncidentFromMap(row)).ActionCandidates[1]

	if cancel.ActionType != "cancel_backend" || cancel.BlockedReason != "" {
		t.Fatalf("cancel = %+v, want an unblocked guarded cancel", cancel)
	}
	if !strings.Contains(cancel.ProposedSQL,
		"a.backend_start = '2026-11-06T09:15:30.123456789Z'::timestamptz") {
		t.Fatalf("script does not pin backend_start:\n%s", cancel.ProposedSQL)
	}

	row["causal_chain"] = []any{map[string]any{"order": float64(2),
		"signal": "lock_contention", "description": "root", "evidence": "pid=4242"}}
	old := cases.ProjectIncident(sourceIncidentFromMap(row)).ActionCandidates[1]
	if old.BlockedReason == "" || old.ProposedSQL != "" {
		t.Fatalf("pre-playbooks-01 row = %+v, want a blocked cancel", old)
	}
}

// migration-05: the queued-DDL marker RCA records on a root blocker
// survives the jsonb read, so the playbook withholds the root's signal.
func TestIncidentChainFromAnyKeepsQueuedDDL(t *testing.T) {
	b := rcaBlocker()
	b.QueuedDDLPID = 200
	got := incidentChainFromAny(storedChain(t, []rca.ChainLink{
		{Order: 2, Signal: "lock_contention", Blocker: b}}))
	if len(got) != 1 || got[0].Blocker == nil || got[0].Blocker.QueuedDDLPID != 200 {
		t.Fatalf("chain = %+v, want the blocker with QueuedDDLPID 200", got)
	}
	plain := incidentChainFromAny(storedChain(t, []rca.ChainLink{
		{Order: 2, Signal: "lock_contention", Blocker: rcaBlocker()}}))
	if plain[0].Blocker.QueuedDDLPID != 0 {
		t.Fatalf("blocker without a queued DDL decoded %d", plain[0].Blocker.QueuedDDLPID)
	}
}
