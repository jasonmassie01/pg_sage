package causal

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-05: a lock investigation probes at least one trigger poll after
// detection, when many chains have cleared. The incident's detection-time
// root blocker (committed as incident_snapshot evidence) then names who
// blocked whom, as a contributing hypothesis only: a detection-time root
// is never conclusive, and a live non-empty graph always wins.

// No concurrent access tests: DiagnoseLock is a pure function of its
// arguments.

var snapProbeAt = time.Date(2026, 11, 11, 10, 0, 45, 0, time.UTC)

var snapBackendStart = time.Date(2026, 11, 11, 9, 58, 0, 0, time.UTC)

// detectedBlocker is pid 4242, idle in transaction and blocking three
// sessions 45 s before the probes ran.
func detectedBlocker(state string) BlockerEvidence {
	return BlockerEvidence{EvidenceID: "S1", PID: 4242, State: state, TotalBlocked: 3,
		ChainDepth: 2, BackendStart: snapBackendStart,
		DetectedAt: snapProbeAt.Add(-45 * time.Second)}
}

// emptyGraph is a usable lock graph with no waits, probed at snapProbeAt.
func emptyGraph() []Observation {
	g := obs("P1", probes.LockGraph)
	g.Result.ObservedAt = snapProbeAt
	return []Observation{g, obs("P2", probes.PreparedXacts)}
}

func TestDiagnoseLock_EmptyGraphNamesTheDetectionBlocker(t *testing.T) {
	d := DiagnoseLock(emptyGraph(), []BlockerEvidence{detectedBlocker("idle in transaction")})
	if d.Conclusive || d.Root != nil {
		t.Fatalf("a detection-time root must never be conclusive: root %+v", d.Root)
	}
	for _, want := range []string{"chain cleared before probing", "at detection pid 4242",
		"idle in transaction", "backend_start 2026-11-11T09:58:00Z", "blocked 3",
		"45 s older than the probes"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason %q lacks %q", d.Reason, want)
		}
	}
	if len(d.Contributing) != 1 || d.Contributing[0].Node != IdleInTxHolder ||
		d.Contributing[0].Status != StatusContributing ||
		d.Contributing[0].Subject != "pid 4242" {
		t.Fatalf("contributing = %+v, want idle_in_tx_holder for pid 4242", d.Contributing)
	}
	citesOnly(t, d.Contributing[0], "S1")
	if !strings.HasPrefix(d.Contributing[0].Support[0].Text, "at detection, ") {
		t.Fatalf("support %q does not say it was seen at detection",
			d.Contributing[0].Support[0].Text)
	}
	if d.Subject != "pid 4242" {
		t.Fatalf("subject = %q, want pid 4242", d.Subject)
	}
	requireRuledOut(t, d, PreparedXactHolder, "no prepared transactions")
}

// A live non-empty graph ignores the snapshot entirely: the same
// diagnosis as without it, even when the snapshot names another pid.
func TestDiagnoseLock_LiveGraphWinsOverTheSnapshot(t *testing.T) {
	base := DiagnoseLock(idleChain("idle in transaction", 75), nil)
	other := detectedBlocker("active")
	other.PID = 9999
	got := DiagnoseLock(idleChain("idle in transaction", 75), []BlockerEvidence{other})
	if !reflect.DeepEqual(base, got) {
		t.Fatalf("the snapshot changed a graph-derived diagnosis:\nwant %+v\ngot  %+v",
			base, got)
	}
	requireRoot(t, got, IdleInTxHolder, 0.85)
	if !got.Conclusive || got.Subject != "pid 10" {
		t.Fatalf("diagnosis = %+v, want conclusive on pid 10", got)
	}
}

// RCA narration passes blockers without a detection time: an empty
// graph keeps today's reason (CHECK: no RCA behaviour change).
func TestDiagnoseLock_BlockersWithoutDetectionTimeKeepTheClearedReason(t *testing.T) {
	b := detectedBlocker("idle in transaction")
	b.DetectedAt = time.Time{}
	d := DiagnoseLock(emptyGraph(), []BlockerEvidence{b})
	if d.Conclusive || d.Reason != "no lock waits at probe time" || len(d.Contributing) != 0 {
		t.Fatalf("diagnosis = %+v, want the unchanged cleared-chain reason", d)
	}
}

// Nil and empty snapshots behave exactly as today.
func TestDiagnoseLock_NoSnapshotKeepsTheClearedReason(t *testing.T) {
	for name, bs := range map[string][]BlockerEvidence{"nil": nil, "empty": {}} {
		d := DiagnoseLock(emptyGraph(), bs)
		if d.Conclusive || d.Reason != "no lock waits at probe time" ||
			len(d.Contributing) != 0 || d.Subject != "" {
			t.Fatalf("%s: diagnosis = %+v, want no lock waits", name, d)
		}
	}
}

// An unavailable lock graph with a detection snapshot is still not
// conclusive (the RCA fallback roots on it; the investigator must not).
func TestDiagnoseLock_UnavailableGraphWithSnapshotIsNeverConclusive(t *testing.T) {
	o := []Observation{unavailable("P1", probes.LockGraph, probes.StatusNoPrivilege),
		obs("P2", probes.PreparedXacts)}
	d := DiagnoseLock(o, []BlockerEvidence{detectedBlocker("idle in transaction")})
	if d.Conclusive || d.Root != nil {
		t.Fatalf("root %+v from detection-time evidence, want none", d.Root)
	}
	if !strings.Contains(d.Reason, "lock graph unavailable") ||
		!strings.Contains(d.Reason, "at detection pid 4242") ||
		strings.Contains(d.Reason, "chain cleared") {
		t.Fatalf("reason = %q", d.Reason)
	}
	if strings.Contains(d.Reason, "older than the probes") {
		t.Fatalf("reason %q dates the snapshot against a graph with no time", d.Reason)
	}
	if len(d.Missing) == 0 || d.Missing[0].ProbeID != probes.LockGraph {
		t.Fatalf("missing = %+v, want lock_graph named", d.Missing)
	}
}

// The RCA fallback (no detection time) still roots on the incident's
// blocker when the graph is unavailable: unchanged behaviour.
func TestDiagnoseLock_RCAFallbackStillRootsWithoutDetectionTime(t *testing.T) {
	b := detectedBlocker("idle in transaction")
	b.DetectedAt = time.Time{}
	o := []Observation{unavailable("P1", probes.LockGraph, probes.StatusNoPrivilege)}
	d := DiagnoseLock(o, []BlockerEvidence{b})
	requireRoot(t, d, IdleInTxHolder, 0.70)
	if !d.Conclusive {
		t.Fatalf("RCA fallback lost its conclusiveness: %+v", d)
	}
}

// An active detection-time holder contradicts the idle hypothesis: no
// contributing hypothesis, still inconclusive, still named in the reason.
func TestDiagnoseLock_ActiveDetectionBlockerContributesNothing(t *testing.T) {
	d := DiagnoseLock(emptyGraph(), []BlockerEvidence{detectedBlocker("active")})
	if d.Conclusive || d.Root != nil || len(d.Contributing) != 0 {
		t.Fatalf("diagnosis = %+v, want inconclusive with no contributing", d)
	}
	requireRuledOut(t, d, IdleInTxHolder, "is active")
	if !strings.Contains(d.Reason, "at detection pid 4242 (active,") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// Boundary: the blocker blocking the most sessions is named; ties go to
// the lower pid. Unknown state and backend start are said, not invented.
func TestDiagnoseLock_SnapshotPicksTheWidestBlocker(t *testing.T) {
	a := detectedBlocker("")
	a.PID, a.TotalBlocked, a.BackendStart = 77, 9, time.Time{}
	b := detectedBlocker("idle in transaction")
	tie := detectedBlocker("idle in transaction")
	tie.PID, tie.TotalBlocked = 76, 9
	d := DiagnoseLock(emptyGraph(), []BlockerEvidence{b, a, tie})
	if !strings.Contains(d.Reason, "at detection pid 76 ") {
		t.Fatalf("reason = %q, want pid 76 (9 blocked, lower pid)", d.Reason)
	}
	d = DiagnoseLock(emptyGraph(), []BlockerEvidence{b, a})
	if !strings.Contains(d.Reason, "at detection pid 77 (state unknown, "+
		"backend_start unknown) blocked 9") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// A snapshot dated after the probe (clock skew) is not given a negative age.
func TestDiagnoseLock_SnapshotAfterTheProbeHasNoAge(t *testing.T) {
	b := detectedBlocker("idle in transaction")
	b.DetectedAt = snapProbeAt.Add(time.Second)
	d := DiagnoseLock(emptyGraph(), []BlockerEvidence{b})
	if strings.Contains(d.Reason, "older than the probes") {
		t.Fatalf("reason = %q, want no age", d.Reason)
	}
	if !strings.Contains(d.Reason, "at detection pid 4242") {
		t.Fatalf("reason = %q", d.Reason)
	}
}
