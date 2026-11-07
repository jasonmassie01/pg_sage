package sre

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Stale evidence (Sage SRE M4, Codex canonical code stale_evidence): an
// observation more than MaxEvidenceSpread older than the investigation's
// newest observation (a run resumed long after its first step, or a
// replayed snapshot) describes a past state. It is never a basis for a
// root cause: the diagnosis treats it as missing evidence with reason
// stale_evidence. Observations within the spread are untouched.

var staleT0 = time.Date(2026, 9, 14, 3, 12, 0, 0, time.UTC)

func idleGraph(at time.Time) probes.Result {
	return probes.Result{ProbeID: probes.LockGraph, Status: probes.StatusOK, ObservedAt: at,
		Rows: []probes.Row{{"waiter_pid": int64(11), "blocker_pid": int64(10),
			"blocker_kind": "backend", "blocker_state": "idle in transaction",
			"lock_type": "transactionid", "requested_mode": "ShareLock",
			"blocker_xact_age_s": 340.0}}}
}

func lockObs(graphAt time.Time) []causal.Observation {
	empty := func(id probes.ID) probes.Result {
		return probes.Result{ProbeID: id, Status: probes.StatusEmpty, ObservedAt: staleT0}
	}
	return []causal.Observation{{EvidenceID: "E1", Result: idleGraph(graphAt)},
		{EvidenceID: "E2", Result: empty(probes.PreparedXacts)},
		{EvidenceID: "E3", Result: empty(probes.LongTransactions)},
		{EvidenceID: "E4", Result: empty(probes.SageActions)}}
}

func TestDiagnose_StaleLockGraphIsMissingNotARoot(t *testing.T) {
	inv := Investigation{TriggerKind: TriggerLock}
	d := diagnose(inv, lockObs(staleT0.Add(-45*time.Minute)))
	if d.Conclusive || d.Root != nil {
		t.Fatalf("a 45-minute-old lock graph concluded %+v", d.Root)
	}
	if !hasMissing(d, probes.LockGraph, "stale_evidence") {
		t.Fatalf("missing %+v lacks lock_graph stale_evidence", d.Missing)
	}
}

func TestDiagnose_SpreadBoundary(t *testing.T) {
	inv := Investigation{TriggerKind: TriggerLock}
	within := diagnose(inv, lockObs(staleT0.Add(-MaxEvidenceSpread)))
	if !within.Conclusive || within.Root == nil || within.Root.Node != causal.IdleInTxHolder {
		t.Fatalf("a graph exactly %s older must still count: %+v", MaxEvidenceSpread,
			within)
	}
	beyond := diagnose(inv, lockObs(staleT0.Add(-MaxEvidenceSpread-time.Second)))
	if beyond.Conclusive {
		t.Fatalf("a graph %s older concluded", MaxEvidenceSpread+time.Second)
	}
}

func TestFreshObservations_KeepsIdentityAndDropsOnlyRows(t *testing.T) {
	obs := lockObs(staleT0.Add(-time.Hour))
	out := freshObservations(obs)
	if len(out) != len(obs) {
		t.Fatalf("%d observations, want %d", len(out), len(obs))
	}
	s := out[0]
	if s.EvidenceID != "E1" || s.Result.ProbeID != probes.LockGraph ||
		s.Result.Status != probes.StatusError || s.Result.Reason != "stale_evidence" ||
		len(s.Result.Rows) != 0 || !s.Result.ObservedAt.Equal(staleT0.Add(-time.Hour)) {
		t.Fatalf("stale observation became %+v", s)
	}
	for i := 1; i < len(out); i++ {
		if out[i].Result.Status != probes.StatusEmpty {
			t.Fatalf("fresh observation %d changed: %+v", i, out[i])
		}
	}
	if obs[0].Result.Status != probes.StatusOK || len(obs[0].Result.Rows) != 1 {
		t.Fatal("freshObservations mutated its input")
	}
}

func TestFreshObservations_EmptyAndUndated(t *testing.T) {
	if got := freshObservations(nil); len(got) != 0 {
		t.Fatalf("nil -> %+v", got)
	}
	undated := []causal.Observation{{EvidenceID: "E1", Result: probes.Result{
		ProbeID: probes.LockGraph, Status: probes.StatusEmpty}},
		{EvidenceID: "E2", Result: idleGraph(staleT0)}}
	out := freshObservations(undated)
	if out[0].Result.Status != probes.StatusEmpty || out[1].Result.Status != probes.StatusOK {
		t.Fatalf("an observation without a time must not be judged stale: %+v", out)
	}
}

// A stale first connection sample leaves one usable sample: growth
// cannot be measured (resume after a pause, replayed snapshot).
func TestDiagnose_StaleFirstConnectionSampleIsNotCompared(t *testing.T) {
	sample := func(at time.Time, idle int64) causal.Observation {
		return causal.Observation{EvidenceID: at.String(), Result: probes.Result{
			ProbeID: probes.ConnectionSaturation, Status: probes.StatusOK, ObservedAt: at,
			Rows: []probes.Row{{"in_current_database": true, "application_name": "etl",
				"client_addr": "10.0.0.9", "state": "idle", "backends": idle,
				"waiting_on_lock": int64(0), "max_connections": int64(200),
				"reserved_connections": int64(3), "total_client_backends": idle,
				"server_started_at": staleT0.Add(-24 * time.Hour)}}}}
	}
	inv := Investigation{TriggerKind: TriggerConnections}
	d := diagnose(inv, []causal.Observation{sample(staleT0.Add(-35*time.Minute), 2),
		sample(staleT0, 9)})
	if d.Conclusive {
		t.Fatalf("growth measured against a stale sample: %+v", d.Root)
	}
	if !hasMissing(d, probes.ConnectionSaturation, "stale_evidence") {
		t.Fatalf("missing %+v lacks the stale sample", d.Missing)
	}
}

func hasMissing(d causal.Diagnosis, id probes.ID, reason string) bool {
	for _, m := range d.Missing {
		if m.ProbeID == id && m.Reason == reason {
			return true
		}
	}
	return false
}

// Stale resume (sre-02): staleness against now, not only within a run.
// A run resumed after a restart or a pause judges its stored evidence by
// the age of the newest dated row at the time it resumes.

func dated(at time.Time) Evidence { return Evidence{ProbeID: "lock_graph", ObservedAt: at} }

func TestEvidenceAge_SpreadBoundary(t *testing.T) {
	now := staleT0
	at := ageOfEvidence([]Evidence{dated(now.Add(-MaxEvidenceSpread))}, now)
	if at.stale() || at.age != MaxEvidenceSpread ||
		!at.newest.Equal(now.Add(-MaxEvidenceSpread)) {
		t.Fatalf("evidence exactly %s old = %+v, stale %v; want usable", MaxEvidenceSpread,
			at, at.stale())
	}
	past := ageOfEvidence([]Evidence{dated(now.Add(-MaxEvidenceSpread - time.Second))}, now)
	if !past.stale() || past.age != MaxEvidenceSpread+time.Second {
		t.Fatalf("evidence 1s past the spread = %+v, stale %v; want stale", past, past.stale())
	}
}

func TestEvidenceAge_NewestRowDecides(t *testing.T) {
	now := staleT0
	ev := []Evidence{dated(now.Add(-2 * time.Hour)), dated(now.Add(-30 * time.Second)),
		dated(now.Add(-time.Hour))}
	a := ageOfEvidence(ev, now)
	if a.stale() || a.age != 30*time.Second || !a.newest.Equal(now.Add(-30*time.Second)) {
		t.Fatalf("age = %+v, stale %v; want the 30s-old row to decide", a, a.stale())
	}
}

func TestEvidenceAge_UndatedRowsAreNotNewest(t *testing.T) {
	now := staleT0
	ev := []Evidence{{ProbeID: "long_transactions"}, dated(now.Add(-time.Hour))}
	a := ageOfEvidence(ev, now)
	if !a.stale() || a.age != time.Hour {
		t.Fatalf("an undated row made hour-old evidence current: %+v", a)
	}
	none := ageOfEvidence([]Evidence{{ProbeID: "long_transactions"}}, now)
	if none.stale() || !none.newest.IsZero() || none.age != 0 {
		t.Fatalf("only undated evidence = %+v; want no age and not stale", none)
	}
}

// Nil/empty: a fresh queued run has no evidence and is never stale.
func TestEvidenceAge_NoEvidence(t *testing.T) {
	for _, ev := range [][]Evidence{nil, {}} {
		a := ageOfEvidence(ev, staleT0)
		if a.stale() || !a.newest.IsZero() || a.age != 0 {
			t.Fatalf("no evidence = %+v", a)
		}
		var s Summary
		a.apply(&s)
		if s.EvidenceNewestAt != nil || s.EvidenceAgeS != 0 {
			t.Fatalf("no evidence set the summary: %+v", s)
		}
	}
}

// Boundary: evidence dated after now (sidecar and database clocks
// disagree) is current, never a negative age.
func TestEvidenceAge_FutureEvidenceIsCurrent(t *testing.T) {
	a := ageOfEvidence([]Evidence{dated(staleT0.Add(90 * time.Second))}, staleT0)
	if a.stale() || a.age != 0 {
		t.Fatalf("future evidence = %+v, stale %v", a, a.stale())
	}
	var s Summary
	a.apply(&s)
	if s.EvidenceAgeS != 0 || s.EvidenceNewestAt == nil ||
		!s.EvidenceNewestAt.Equal(staleT0.Add(90*time.Second)) {
		t.Fatalf("summary = %+v", s)
	}
}

func TestEvidenceAge_SetsTheSummary(t *testing.T) {
	newest := staleT0.Add(-(61*time.Minute + 59*time.Second))
	var s Summary
	ageOfEvidence([]Evidence{dated(newest)}, staleT0).apply(&s)
	if s.EvidenceNewestAt == nil || !s.EvidenceNewestAt.Equal(newest) ||
		s.EvidenceAgeS != 3719 {
		t.Fatalf("summary = newest %v age %d; want %v and 3719", s.EvidenceNewestAt,
			s.EvidenceAgeS, newest)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"evidence_newest_at":"2026-09-14T02:10:01Z"`) ||
		!strings.Contains(string(raw), `"evidence_age_s":3719`) {
		t.Fatalf("summary JSON %s lacks the evidence age", raw)
	}
	plain, _ := json.Marshal(Summary{})
	if strings.Contains(string(plain), "evidence_") {
		t.Fatalf("a summary without evidence carries an age: %s", plain)
	}
}

// No concurrent access test here: ageOfEvidence is a pure function of
// its arguments; the concurrent stale resume runs on the store.
