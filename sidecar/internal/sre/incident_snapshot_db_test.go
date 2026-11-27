package sre

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/signalid"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-05 on real PostgreSQL: a lock investigation of an incident whose
// chain cleared before probing reads the incident's detection-time root
// blocker from sage.incidents (one bounded read, the trigger adapter's
// database scope), commits it as incident_snapshot evidence under the
// lease, and names it in an inconclusive diagnosis (CHECK-31).

// No concurrent access tests: one investigation runs per lease, and the
// evidence insert is the existing CommitStep under leaseGuard (covered
// by TestCoordinator_TwoCoordinatorsRaceTheSameIncident).

const snapDB = "snapdb"

// plantedBlockerChain is a recorded idle-in-transaction root blocker with
// secrets planted in its application name, query and free text.
const plantedBlockerChain = `[{"order":1,"signal":"lock_contention",
 "description":"pid 4242 blocks 3 sessions password=hunter2",
 "evidence":"alice@example.com","blocker":{"pid":4242,
 "backend_start":"2026-11-11T09:58:00Z","query_id":77,
 "application_name":"psql password=hunter2 postgres://app:Pa55w0rd@db.internal/prod",
 "query":"UPDATE accounts SET pw = 'Secr3tPw' WHERE id = 1",
 "state":"idle in transaction","total_blocked":3,"chain_depth":2}}]`

func insertLockIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db,
	chain string, detectedAgo time.Duration) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, causal_chain, signal_ids, source, confidence,
		 database_name, detected_at, last_detected_at)
		VALUES ('warning', 'fixture', $1::jsonb, $2, 'deterministic', 1.0, $3,
		        now() - $4::interval - interval '10 seconds', now() - $4::interval)
		RETURNING id::text`, chain, []string{signalid.LockContention}, db,
		detectedAgo.String()).Scan(&id); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.incidents WHERE id = $1::uuid", id)
	})
	return id
}

// clearedChainRunner is a lock plan whose live probes all came back empty.
func clearedChainRunner() *scriptedRunner {
	r := newScriptedRunner()
	for _, id := range []probes.ID{probes.LockGraph, probes.PreparedXacts,
		probes.LongTransactions, probes.SageActions} {
		r.script(id, rows(id))
	}
	return r
}

// snapshotCoordinator is testCoordinator with the incident snapshot read
// from pool's sage.incidents, scoped to snapDB.
func snapshotCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	pool *pgxpool.Pool, runner ProbeRunner) *Coordinator {
	t.Helper()
	cfg := DefaultCoordinatorConfig("test:" + string(NewUUID()))
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner, Config: cfg,
		LogFn: func(string, string, ...any) {}, Incidents: NewPGTriggerSource(pool, snapDB)})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := c.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c
}

func snapshotRows(t *testing.T, ctx context.Context, st *PostgresStore,
	inv Investigation) []Evidence {
	t.Helper()
	ev, err := st.Evidence(ctx, inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	var out []Evidence
	for _, e := range ev {
		if e.ProbeID == string(IncidentSnapshotProbeID) {
			out = append(out, e)
		}
	}
	return out
}

func hypothesisNamed(t *testing.T, ctx context.Context, st *PostgresStore, inv Investigation,
	node string) *HypothesisRecord {
	t.Helper()
	hs, err := st.Hypotheses(ctx, inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("hypotheses: %v", err)
	}
	for i := range hs {
		if hs[i].Node == node {
			return &hs[i]
		}
	}
	return nil
}

// Happy path (CHECK-31): today this says "no lock waits at probe time".
func TestIncidentSnapshot_ClearedChainNamesTheDetectionBlocker(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	id := insertLockIncident(t, ctx, pool, snapDB, plantedBlockerChain, 20*time.Second)
	c := snapshotCoordinator(t, ctx, st, pool, clearedChainRunner())
	inv := startAndRun(t, ctx, c, lockTrigger(id))
	if inv.State != StateInconclusive || inv.Summary.Conclusive || inv.Summary.Root != "" {
		t.Fatalf("investigation = %s conclusive %v root %q, want inconclusive", inv.State,
			inv.Summary.Conclusive, inv.Summary.Root)
	}
	for _, want := range []string{"chain cleared before probing", "at detection pid 4242",
		"idle in transaction", "backend_start 2026-11-11T09:58:00Z", "blocked 3",
		"s older than the probes"} {
		if !strings.Contains(inv.Summary.Reason, want) {
			t.Errorf("reason %q lacks %q", inv.Summary.Reason, want)
		}
	}
	snaps := snapshotRows(t, ctx, st, inv)
	if len(snaps) != 1 || snaps[0].StepKey != "step-1" ||
		snaps[0].CapabilityState != "available" || inv.ProbeCount != 5 {
		t.Fatalf("snapshot evidence = %+v (probes %d), want one available step-1 row",
			snaps, inv.ProbeCount)
	}
	var lastDetected time.Time
	if err := pool.QueryRow(ctx, `SELECT last_detected_at FROM sage.incidents
		WHERE id = $1::uuid`, id).Scan(&lastDetected); err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if !snaps[0].ObservedAt.Equal(lastDetected) {
		t.Fatalf("snapshot observed at %v, want the incident's last detection %v",
			snaps[0].ObservedAt, lastDetected)
	}
	idle := hypothesisNamed(t, ctx, st, inv, "idle_in_tx_holder")
	if idle == nil || idle.Status != HypothesisContributing || len(idle.Support) == 0 ||
		idle.Support[0].EvidenceID != snaps[0].ID {
		t.Fatalf("idle hypothesis = %+v, want contributing, citing the snapshot", idle)
	}
	text := string(snaps[0].Payload)
	for _, k := range append(leakCanaries, "application_name", "UPDATE", "accounts") {
		if strings.Contains(text, k) {
			t.Errorf("%q leaked into the snapshot evidence: %s", k, text)
		}
	}
	if !strings.Contains(text, `"query_sha"`) || !snaps[0].VerifyHash() {
		t.Fatalf("payload %s: want the query's hash and a verified digest", text)
	}
}

// Nil/empty: an incident without a causal chain commits no snapshot and
// concludes exactly as today.
func TestIncidentSnapshot_NoChainCommitsNoSnapshot(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	id := insertLockIncident(t, ctx, pool, snapDB, `[]`, 20*time.Second)
	c := snapshotCoordinator(t, ctx, st, pool, clearedChainRunner())
	inv := startAndRun(t, ctx, c, lockTrigger(id))
	if inv.State != StateInconclusive || inv.Summary.Reason != "no lock waits at probe time" ||
		inv.ProbeCount != 4 || len(snapshotRows(t, ctx, st, inv)) != 0 {
		t.Fatalf("investigation = %s %q probes %d", inv.State, inv.Summary.Reason,
			inv.ProbeCount)
	}
}

// Nil/empty: a non-lock trigger of the same incident reads no snapshot.
func TestIncidentSnapshot_NonLockTriggerCommitsNoSnapshot(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	id := insertLockIncident(t, ctx, pool, snapDB, plantedBlockerChain, 20*time.Second)
	c := snapshotCoordinator(t, ctx, st, pool, leakRunner())
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "case:conn:" + id, IncidentID: id,
		Kind: TriggerConnections, Subject: "incident " + id, IdempotencyKey: "incident:" + id})
	if inv.State != StateConcluded || len(snapshotRows(t, ctx, st, inv)) != 0 {
		t.Fatalf("investigation = %s, snapshot rows %d", inv.State,
			len(snapshotRows(t, ctx, st, inv)))
	}
}

// Invalid input: a non-numeric pid is recorded as error evidence, never
// as a blocker.
func TestIncidentSnapshot_MalformedChainIsErrorEvidence(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	id := insertLockIncident(t, ctx, pool, snapDB,
		`[{"blocker":{"pid":"abc","state":"idle in transaction","total_blocked":3}}]`,
		20*time.Second)
	c := snapshotCoordinator(t, ctx, st, pool, clearedChainRunner())
	inv := startAndRun(t, ctx, c, lockTrigger(id))
	snaps := snapshotRows(t, ctx, st, inv)
	if len(snaps) != 1 || snaps[0].ReasonCode != ReasonMalformedChain ||
		snaps[0].CapabilityState != "unknown" {
		t.Fatalf("snapshot evidence = %+v, want one malformed_causal_chain error", snaps)
	}
	if inv.State != StateInconclusive || inv.Summary.Reason != "no lock waits at probe time" {
		t.Fatalf("investigation = %s %q", inv.State, inv.Summary.Reason)
	}
	found := false
	for _, m := range inv.Summary.Missing {
		found = found || (m.ProbeID == string(IncidentSnapshotProbeID) &&
			m.Reason == ReasonMalformedChain)
	}
	hs, _ := st.Hypotheses(ctx, inv.Scope, inv.ID)
	for _, h := range hs {
		if h.Status == HypothesisContributing {
			t.Fatalf("hypothesis %s contributing from a malformed chain", h.Node)
		}
	}
	if !found {
		t.Fatalf("missing = %+v, want the malformed snapshot named", inv.Summary.Missing)
	}
}

// Fleet scope: an incident of another database, an unknown incident and
// an id that is not a UUID are not found; none is an error.
func TestIncidentSnapshot_ReadIsScopedToTheDatabase(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	other := insertLockIncident(t, ctx, pool, "otherdb", plantedBlockerChain, 0)
	mine := insertLockIncident(t, ctx, pool, snapDB, plantedBlockerChain, 0)
	src := NewPGTriggerSource(pool, snapDB)
	for name, id := range map[string]string{"other database": other,
		"unknown": string(NewUUID()), "not a uuid": "inc-1", "empty": ""} {
		if snap, found, err := src.IncidentSnapshot(ctx, id); err != nil || found {
			t.Errorf("%s: snapshot %+v found %v err %v, want not found", name, snap, found, err)
		}
	}
	snap, found, err := src.IncidentSnapshot(ctx, mine)
	if err != nil || !found || !strings.Contains(string(snap.CausalChain), `"pid": 4242`) ||
		snap.DetectedAt.IsZero() {
		t.Fatalf("own incident: %+v found %v err %v", snap, found, err)
	}
	var nilSrc *PGTriggerSource
	if _, _, err := nilSrc.IncidentSnapshot(ctx, mine); err == nil {
		t.Fatal("a source without a database must say so")
	}
}

// Error propagation: a read that fails (here, a cancelled context) is an
// error, not "not found".
func TestIncidentSnapshot_ReadFailureIsAnError(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	id := insertLockIncident(t, ctx, pool, snapDB, plantedBlockerChain, 0)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, found, err := NewPGTriggerSource(pool, snapDB).IncidentSnapshot(cancelled, id)
	if err == nil || found || !strings.Contains(err.Error(), "incident") {
		t.Fatalf("found %v err %v, want an error naming the incident read", found, err)
	}
}

// State transitions: a run cut off after the snapshot step resumes with
// the same evidence and the same diagnosis, even though the incident's
// chain changed meanwhile (the snapshot is committed, never re-read).
func TestIncidentSnapshot_ResumeKeepsTheCommittedSnapshot(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	id := insertLockIncident(t, ctx, pool, snapDB, plantedBlockerChain, 20*time.Second)
	runner := clearedChainRunner()
	c := snapshotCoordinator(t, ctx, st, pool, runner)
	scope, _ := c.Scope()
	inv, _, err := c.Start(ctx, lockTrigger(id))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	dead, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	plan, _ := c.plan(TriggerLock)
	if _, err := c.collect(ctx, dead, inv, plan); err != nil {
		t.Fatalf("dead worker collect: %v", err)
	}
	expireLease(t, ctx, pool, dead)
	if _, err := pool.Exec(ctx, `UPDATE sage.incidents SET causal_chain =
		'[{"blocker":{"pid":7777,"state":"active","total_blocked":1}}]'::jsonb
		WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("change chain: %v", err)
	}
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := st.Get(ctx, scope, inv.ID)
	if got.State != StateInconclusive ||
		!strings.Contains(got.Summary.Reason, "at detection pid 4242") {
		t.Fatalf("resumed = %s %q, want the committed snapshot's pid 4242", got.State,
			got.Summary.Reason)
	}
	if n := len(snapshotRows(t, ctx, st, got)); n != 1 || runner.calls[probes.LockGraph] != 1 {
		t.Fatalf("%d snapshot rows, lock_graph ran %d times; want 1 and 1", n,
			runner.calls[probes.LockGraph])
	}
}

// Integration: a live non-empty graph wins over the snapshot; the
// investigation concludes on the graph's root as without one.
func TestIncidentSnapshot_LiveGraphStillWins(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	chain := strings.ReplaceAll(plantedBlockerChain, "4242", "9999")
	id := insertLockIncident(t, ctx, pool, snapDB, chain, 20*time.Second)
	c := snapshotCoordinator(t, ctx, st, pool, idleChainRunner())
	inv := startAndRun(t, ctx, c, lockTrigger(id))
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.Summary.Subject != "pid 4242" || strings.Contains(inv.Summary.Reason, "9999") {
		t.Fatalf("investigation = %s root %q subject %q reason %q", inv.State,
			inv.Summary.Root, inv.Summary.Subject, inv.Summary.Reason)
	}
	if len(snapshotRows(t, ctx, st, inv)) != 1 {
		t.Fatal("the snapshot is still recorded as evidence")
	}
}
