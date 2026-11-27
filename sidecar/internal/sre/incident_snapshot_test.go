package sre

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-05 (unit): the detection snapshot of a lock incident becomes one
// incident_snapshot evidence row (at most 5 blockers, the query reduced
// to its hash, no free text) and, through diagnose, a contributing
// hypothesis when the live graph is empty. It is judged stale like any
// other evidence.

// No concurrent access tests: snapshotResult and diagnose are pure
// functions of their arguments; the DB tests cover the committed path.

var snapProbeTime = time.Date(2026, 11, 11, 10, 5, 0, 0, time.UTC)

const snapChain = `[{"order":1,"signal":"lock_contention","description":"pid 4242 blocks 3",
 "evidence":"lock_chain","blocker":{"pid":4242,"backend_start":"2026-11-11T09:58:00Z",
 "query_start":"2026-11-11T09:59:00Z","query_id":77,"query_sha":"0123456789abcdef",
 "state":"idle in transaction","total_blocked":3,"chain_depth":2}}]`

func snapshotEvidence(t *testing.T, chain string, detected time.Time) probes.Result {
	t.Helper()
	res, ok := snapshotResult(IncidentSnapshot{CausalChain: []byte(chain),
		DetectedAt: detected})
	if !ok {
		t.Fatalf("no snapshot from %s", chain)
	}
	return res
}

// emptyLockStep is the lock plan's step with every live probe empty.
func emptyLockStep(at time.Time) []probes.Result {
	var out []probes.Result
	for _, id := range []probes.ID{probes.LockGraph, probes.PreparedXacts,
		probes.LongTransactions, probes.SageActions} {
		r := rows(id)
		r.ObservedAt = at
		out = append(out, r)
	}
	return out
}

func lockDiagnosisWith(t *testing.T, snap probes.Result) causal.Diagnosis {
	t.Helper()
	ev := fixtureEvidence(t, append([]probes.Result{snap}, emptyLockStep(snapProbeTime)...)...)
	return diagnoseEvidence(t, Investigation{TriggerKind: TriggerLock}, ev)
}

func missingSnapshot(d causal.Diagnosis) (causal.Missing, bool) {
	for _, m := range d.Missing {
		if m.ProbeID == IncidentSnapshotProbeID {
			return m, true
		}
	}
	return causal.Missing{}, false
}

func TestSnapshotResult_RecordsTheRootBlockerIdentity(t *testing.T) {
	detected := snapProbeTime.Add(-30 * time.Second)
	res := snapshotEvidence(t, snapChain, detected)
	if res.ProbeID != IncidentSnapshotProbeID || res.Status != probes.StatusOK ||
		!res.ObservedAt.Equal(detected) || len(res.Rows) != 1 {
		t.Fatalf("snapshot = %+v", res)
	}
	r := res.Rows[0]
	if r["pid"] != int64(4242) || r["state"] != "idle in transaction" ||
		r["total_blocked"] != int64(3) || r["chain_depth"] != int64(2) ||
		r["query_sha"] != "0123456789abcdef" {
		t.Fatalf("row = %+v", r)
	}
	if bs, ok := r["backend_start"].(time.Time); !ok ||
		!bs.Equal(time.Date(2026, 11, 11, 9, 58, 0, 0, time.UTC)) {
		t.Fatalf("backend_start = %#v", r["backend_start"])
	}
}

// Happy path through diagnose: an empty live graph names the detection
// blocker, inconclusive, citing the snapshot evidence.
func TestDiagnose_EmptyGraphUsesTheDetectionSnapshot(t *testing.T) {
	d := lockDiagnosisWith(t, snapshotEvidence(t, snapChain, snapProbeTime.Add(-30*time.Second)))
	if d.Conclusive || d.Root != nil || !strings.Contains(d.Reason, "at detection pid 4242") ||
		!strings.Contains(d.Reason, "30 s older than the probes") {
		t.Fatalf("diagnosis reason %q root %+v", d.Reason, d.Root)
	}
	if len(d.Contributing) != 1 || d.Contributing[0].Node != causal.IdleInTxHolder {
		t.Fatalf("contributing = %+v", d.Contributing)
	}
	c := conclusionOf(d)
	if c.State != StateInconclusive || c.Summary.Conclusive || c.Summary.Root != "" {
		t.Fatalf("conclusion = %+v", c.Summary)
	}
}

// Boundary: 4m59s older than the newest probe is used; 5m01s is stale,
// shown as missing and not used.
func TestDiagnose_SnapshotStalenessBoundary(t *testing.T) {
	fresh := lockDiagnosisWith(t, snapshotEvidence(t, snapChain,
		snapProbeTime.Add(-(4*time.Minute+59*time.Second))))
	if !strings.Contains(fresh.Reason, "at detection pid 4242") {
		t.Fatalf("4m59s: reason %q, want the snapshot used", fresh.Reason)
	}
	if m, ok := missingSnapshot(fresh); ok {
		t.Fatalf("4m59s: snapshot reported missing: %+v", m)
	}
	stale := lockDiagnosisWith(t, snapshotEvidence(t, snapChain,
		snapProbeTime.Add(-(5*time.Minute+time.Second))))
	if stale.Reason != "no lock waits at probe time" || len(stale.Contributing) != 0 {
		t.Fatalf("5m01s: diagnosis %q contributing %+v, want the snapshot unused",
			stale.Reason, stale.Contributing)
	}
	m, ok := missingSnapshot(stale)
	if !ok || m.Status != probes.StatusError || m.Reason != "detection snapshot too old" {
		t.Fatalf("5m01s: missing = %+v, want detection snapshot too old", stale.Missing)
	}
}

// Invalid input: a non-numeric pid (or any blocker that is not a
// root-blocker identity) is error evidence, never a blocker.
func TestSnapshotResult_MalformedChainIsErrorEvidence(t *testing.T) {
	for name, chain := range map[string]string{
		"non-numeric pid": `[{"blocker":{"pid":"abc","state":"idle in transaction"}}]`,
		"zero pid":        `[{"blocker":{"pid":0,"state":"idle in transaction"}}]`,
		"negative count":  `[{"blocker":{"pid":5,"total_blocked":-1}}]`,
		"not an array":    `{"blocker":{"pid":5}}`,
		"bad time":        `[{"blocker":{"pid":5,"backend_start":"yesterday"}}]`,
	} {
		res := snapshotEvidence(t, chain, snapProbeTime)
		if res.Status != probes.StatusError || res.Reason != ReasonMalformedChain ||
			len(res.Rows) != 0 {
			t.Errorf("%s: snapshot = %+v, want malformed_causal_chain error", name, res)
		}
		if strings.Contains(res.Error, "abc") || strings.Contains(res.Error, "yesterday") {
			t.Errorf("%s: error %q echoes incident text", name, res.Error)
		}
		d := lockDiagnosisWith(t, res)
		m, ok := missingSnapshot(d)
		if d.Reason != "no lock waits at probe time" || len(d.Contributing) != 0 || !ok ||
			m.Reason != ReasonMalformedChain {
			t.Errorf("%s: diagnosis %q missing %+v", name, d.Reason, d.Missing)
		}
	}
}

// Nil/empty: no chain, an empty chain or a chain without a blocker
// commits no snapshot at all.
func TestSnapshotResult_NoBlockerCommitsNothing(t *testing.T) {
	for name, chain := range map[string]string{"nil": "", "null": "null", "empty": "[]",
		"no blocker": `[{"order":1,"signal":"idle_in_tx_elevated","blocker":null}]`} {
		if res, ok := snapshotResult(IncidentSnapshot{CausalChain: []byte(chain),
			DetectedAt: snapProbeTime}); ok {
			t.Errorf("%s: snapshot %+v, want none", name, res)
		}
	}
}

// Boundary: at most 5 blockers, the most blocking first (ties: lower pid).
func TestSnapshotResult_KeepsTheFiveWidestBlockers(t *testing.T) {
	var links []string
	for i := 1; i <= 7; i++ {
		links = append(links, `{"blocker":{"pid":`+itoa(int64(100+i))+
			`,"state":"active","total_blocked":`+itoa(int64(i%4))+`}}`)
	}
	res := snapshotEvidence(t, "["+strings.Join(links, ",")+"]", snapProbeTime)
	if len(res.Rows) != maxSnapshotBlockers {
		t.Fatalf("%d rows, want %d", len(res.Rows), maxSnapshotBlockers)
	}
	want := []int64{103, 107, 102, 106, 101}
	for i, r := range res.Rows {
		if r["pid"] != want[i] {
			t.Fatalf("row %d pid %v, want order %v (rows %+v)", i, r["pid"], want, res.Rows)
		}
	}
}

// Security: secrets planted in the incident's application name, query,
// state and free text never reach the evidence payload; the query is
// kept only as its hash.
func TestSnapshotResult_PlantedSecretsNeverReachThePayload(t *testing.T) {
	query := "SELECT * FROM users WHERE pw = 'Secr3tPw' -- postgres://app:Pa55w0rd@db.internal"
	chain := `[{"description":"alice@example.com password=hunter2",
	  "evidence":"` + fakeGitHubToken + `","blocker":{"pid":4242,
	  "application_name":"psql password=hunter2 postgres://app:Pa55w0rd@db.internal/prod",
	  "query":"` + query + `","state":"idle in transaction password=hunter2",
	  "locked_relation":"public.patients_ssn","total_blocked":2}}]`
	res := snapshotEvidence(t, chain, snapProbeTime)
	payload, err := canonicalPayload(res)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	text := string(payload)
	for _, k := range append(leakCanaries, "application_name", "SELECT", "patients_ssn",
		"description") {
		if strings.Contains(text, k) {
			t.Errorf("%q leaked into the snapshot payload: %s", k, text)
		}
	}
	sum := sha256.Sum256([]byte(query))
	if res.Rows[0]["query_sha"] != hex.EncodeToString(sum[:8]) {
		t.Fatalf("query_sha = %v, want the query's hash", res.Rows[0]["query_sha"])
	}
	if res.Rows[0]["state"] != "unknown" {
		t.Fatalf("state = %v, want an unrecognised state recorded as unknown",
			res.Rows[0]["state"])
	}
}

// A non-lock trigger never reads a snapshot, and a lock investigation
// without an incident id or a snapshot source reads none either.
func TestIncidentSnapshotRead_OnlyForLockIncidents(t *testing.T) {
	src := &countingSnapshots{snap: IncidentSnapshot{CausalChain: []byte(snapChain),
		DetectedAt: snapProbeTime}, found: true}
	c := &Coordinator{incidents: src, now: func() time.Time { return snapProbeTime },
		logFn: func(string, string, ...any) {}}
	ctx := t.Context()
	for _, inv := range []Investigation{
		{TriggerKind: TriggerConnections, IncidentID: string(NewUUID())},
		{TriggerKind: TriggerLock},
	} {
		if got := c.incidentSnapshot(ctx, inv); len(got) != 0 {
			t.Fatalf("%s: snapshot %+v, want none", inv.TriggerKind, got)
		}
	}
	if src.calls != 0 {
		t.Fatalf("source read %d times, want 0", src.calls)
	}
	got := c.incidentSnapshot(ctx, Investigation{TriggerKind: TriggerLock,
		IncidentID: string(NewUUID())})
	if len(got) != 1 || got[0].ProbeID != IncidentSnapshotProbeID || src.calls != 1 {
		t.Fatalf("lock snapshot = %+v after %d reads", got, src.calls)
	}
	none := &Coordinator{now: time.Now, logFn: c.logFn}
	if got := none.incidentSnapshot(ctx, Investigation{TriggerKind: TriggerLock,
		IncidentID: "x"}); len(got) != 0 {
		t.Fatalf("no source: snapshot %+v", got)
	}
}

// Error propagation: a failed read is error evidence naming the read,
// not a silent gap and not an investigation failure.
func TestIncidentSnapshotRead_FailureIsErrorEvidence(t *testing.T) {
	src := &countingSnapshots{err: errSnapshotTest}
	var logged []string
	c := &Coordinator{incidents: src, now: func() time.Time { return snapProbeTime },
		logFn: func(level, msg string, args ...any) { logged = append(logged, level+" "+msg) }}
	got := c.incidentSnapshot(t.Context(), Investigation{TriggerKind: TriggerLock,
		IncidentID: string(NewUUID())})
	if len(got) != 1 || got[0].Status != probes.StatusError ||
		got[0].Reason != ReasonIncidentUnreadable || !got[0].ObservedAt.Equal(snapProbeTime) {
		t.Fatalf("snapshot = %+v, want incident_unreadable error evidence", got)
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "WARN") {
		t.Fatalf("logged %v, want one warning", logged)
	}
	d := lockDiagnosisWith(t, got[0])
	if m, ok := missingSnapshot(d); !ok || m.Reason != ReasonIncidentUnreadable {
		t.Fatalf("missing = %+v", d.Missing)
	}
}

// A snapshot row without a detection time is never used as one (it would
// otherwise read as an RCA blocker the fallback may root on).
func TestDiagnose_UndatedSnapshotIsNotUsed(t *testing.T) {
	res := snapshotEvidence(t, snapChain, snapProbeTime)
	res.ObservedAt = time.Time{}
	payload, err := canonicalPayload(res)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	snap := Evidence{ID: NewUUID(), StepKey: "step-1",
		ProbeID: string(IncidentSnapshotProbeID), Payload: payload}
	ev := fixtureEvidence(t, lockGraphTimeout(), rows(probes.PreparedXacts))
	d := diagnoseEvidence(t, Investigation{TriggerKind: TriggerLock}, append(ev, snap))
	if d.Conclusive || d.Root != nil || len(d.Contributing) != 0 {
		t.Fatalf("undated snapshot used: %+v", d)
	}
	if m, ok := missingSnapshot(d); !ok || m.Reason != "detection time unknown" {
		t.Fatalf("missing = %+v", d.Missing)
	}
}

var errSnapshotTest = errors.New("read sage.incidents: connection refused")

// countingSnapshots is a scripted IncidentSnapshotSource.
type countingSnapshots struct {
	snap  IncidentSnapshot
	found bool
	err   error
	calls int
}

func (s *countingSnapshots) IncidentSnapshot(context.Context, string) (IncidentSnapshot,
	bool, error) {
	s.calls++
	return s.snap, s.found, s.err
}
