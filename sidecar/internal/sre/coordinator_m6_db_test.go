package sre

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// M6 families through the real coordinator and store: scripted probes,
// the persisted conclusion, its hypotheses (CHECK-37, CHECK-38) and the
// model turn on a new family's node ids.

var m6Start = time.Date(2026, 9, 30, 6, 0, 0, 0, time.UTC)

func ckptScript(req, wal int64) probes.Result {
	return rows(probes.CheckpointActivity, probes.Row{"timed_checkpoints": int64(40),
		"requested_checkpoints": req, "checkpointer_stats_reset": m6Start,
		"wal_bytes": wal, "wal_records": int64(100), "wal_fpi": int64(10),
		"wal_stats_reset": nil, "max_wal_size_bytes": int64(32 << 20),
		"checkpoint_timeout_s": int64(300), "checkpoint_completion_target": 0.9,
		"server_started_at": m6Start, "backend_fsyncs": int64(0)})
}

func checkpointRunner() *scriptedRunner {
	return newScriptedRunner().script(probes.CheckpointActivity, ckptScript(10, 1<<30),
		ckptScript(13, 1<<30+64<<20), ckptScript(16, 1<<30+128<<20))
}

func tempScripts() *scriptedRunner {
	activity := func(files, bytes int64) probes.Result {
		return rows(probes.TempFileActivity, probes.Row{"temp_files": files,
			"temp_bytes": bytes, "stats_reset": m6Start, "work_mem_bytes": int64(4 << 20),
			"temp_file_limit_kb": int64(-1), "server_started_at": m6Start})
	}
	spill := func(calls, blks int64) probes.Result {
		return rows(probes.TempSpillStatements, probes.Row{"queryid": int64(4711),
			"calls": calls, "temp_blks_written": blks, "block_size": int64(8192),
			"stats_reset": m6Start})
	}
	return newScriptedRunner().
		script(probes.TempFileActivity, activity(10, 1<<30), activity(40, 2<<30)).
		script(probes.TempSpillStatements, spill(10, 1000), spill(40, 132072))
}

func replScripts() *scriptedRunner {
	lag := func(replay int64) probes.Result {
		return rows(probes.ReplicationLag, probes.Row{"pid": int64(321),
			"application_name": "standby1", "kind": "physical", "send_backlog_bytes": int64(0),
			"flush_backlog_bytes": int64(0), "replay_backlog_bytes": replay,
			"replay_lag_bytes": replay, "replay_lag_s": 20.0})
	}
	return newScriptedRunner().
		script(probes.ReplicationLag, lag(20<<20), lag(60<<20)).
		script(probes.StandbyReplayState, rows(probes.StandbyReplayState,
			probes.Row{"in_recovery": false, "conflicts": int64(0),
				"server_started_at": m6Start}))
}

func lwScripts() *scriptedRunner {
	hot := rows(probes.LWLockWaits, probes.Row{"wait_event_type": "LWLock",
		"wait_event": "WALWrite", "query_id": int64(9), "in_current_database": true,
		"backends": int64(20), "active_backends": int64(30)})
	return newScriptedRunner().script(probes.LWLockWaits, hot)
}

func m6Trigger(kind TriggerKind, key string) Trigger {
	return Trigger{CaseID: "sre:detector:" + string(kind) + ":test", Kind: kind,
		Subject: string(kind), IdempotencyKey: "m6:" + key}
}

func TestCoordinator_M6FamiliesConclude(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	cases := []struct {
		kind   TriggerKind
		runner *scriptedRunner
		root   string
		probes int
	}{
		{TriggerCheckpoint, checkpointRunner(), "max_wal_size_undersized", 4},
		{TriggerTempFiles, tempScripts(), "repeated_spill_statement", 7},
		{TriggerReplicationLag, replScripts(), "standby_replay_backlog", 7},
		{TriggerLWLock, lwScripts(), "wal_write_contention", 5},
	}
	for _, c := range cases {
		coord, slept := testCoordinator(t, ctx, st, c.runner, nil)
		inv := startAndRun(t, ctx, coord, m6Trigger(c.kind, string(NewUUID())))
		if inv.State != StateConcluded || inv.Summary.Root != c.root ||
			inv.Summary.Family != string(c.kind) || inv.ProbeCount != c.probes {
			t.Fatalf("%s: state %s root %q family %q probes %d (%s)", c.kind, inv.State,
				inv.Summary.Root, inv.Summary.Family, inv.ProbeCount, inv.Summary.Reason)
		}
		if c.runner.total() != c.probes || len(*slept) == 0 {
			t.Fatalf("%s: ran %d probes, slept %v", c.kind, c.runner.total(), *slept)
		}
		checkM6Hypotheses(t, ctx, st, inv)
	}
}

func checkM6Hypotheses(t *testing.T, ctx context.Context, st *PostgresStore,
	inv Investigation) {
	t.Helper()
	hs, err := st.Hypotheses(ctx, inv.Scope, inv.ID)
	if err != nil || len(hs) < 3 {
		t.Fatalf("%s hypotheses = %+v (%v)", inv.TriggerKind, hs, err)
	}
	self := false
	for _, h := range hs {
		if h.RefutationProbe == "" || h.OperatorStep == "" {
			t.Errorf("%s: %s lacks a refutation probe or operator step", inv.TriggerKind,
				h.Node)
		}
		if h.Node == "sage_own_action" {
			self = h.Status == HypothesisRuledOut
		} else if h.Family != string(inv.TriggerKind) {
			t.Errorf("%s: hypothesis %s has family %s", inv.TriggerKind, h.Node, h.Family)
		}
		if h.GraphVersion != "causal-v3" {
			t.Errorf("%s: graph version %s", inv.TriggerKind, h.GraphVersion)
		}
	}
	if !self {
		t.Errorf("%s: no ruled-out pg_sage self-action hypothesis (CHECK-38)", inv.TriggerKind)
	}
}

// The checkpoint plan waits three sample intervals between samples.
func TestCoordinator_CheckpointPlanWaitsLonger(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	coord, slept := testCoordinator(t, ctx, st, checkpointRunner(), nil)
	startAndRun(t, ctx, coord, m6Trigger(TriggerCheckpoint, string(NewUUID())))
	want := 3 * coord.cfg.SampleInterval
	if len(*slept) != 2 || (*slept)[0] != want || (*slept)[1] != want {
		t.Fatalf("slept %v, want two waits of %s", *slept, want)
	}
}

// The model turn ranks and narrates a new family with its graph's node
// ids; the deterministic root stands.
func TestModelTurn_RanksAnM6Family(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(func(body string) string {
		return wireReview{Ranking: []string{"wal_write_contention"},
			Claims: []wireClaim{{Text: "The WALWrite waits peaked at 20 backends.",
				EvidenceIDs: []string{lastAlias(t, body, probes.LWLockWaits)}}}}.json()
	}))
	c, _ := modelCoordinator(t, ctx, st, lwScripts(), m.client())
	inv := startAndRun(t, ctx, c, m6Trigger(TriggerLWLock, "model"))
	if inv.State != StateConcluded || inv.Summary.Root != "wal_write_contention" {
		t.Fatalf("investigation = %s root %q", inv.State, inv.Summary.Root)
	}
	r := inv.Summary.ModelRanking
	if r == nil || strings.Join(r.Nodes, ",") != "wal_write_contention" {
		t.Fatalf("model ranking = %+v; rejected %v", r,
			payloads(t, st, inv, EventModelRejected))
	}
	if n := inv.Summary.Narrative; n == nil || len(n.Claims) != 1 {
		t.Fatalf("narrative = %+v", inv.Summary.Narrative)
	}
	if !strings.Contains(m.body(t, 0), "lwlock_contention incident") {
		t.Fatalf("prompt does not name the family: %s", m.body(t, 0))
	}
}

// lastAlias is the alias of the last stored result of a probe: the
// graph's facts are rendered on the evidence they cite, the last sample.
func lastAlias(t *testing.T, body string, probe probes.ID) string {
	t.Helper()
	re := regexp.MustCompile(`(E\d+) \[` + regexp.QuoteMeta(string(probe)) + ` ok\]`)
	m := re.FindAllStringSubmatch(body, -1)
	if len(m) == 0 {
		t.Errorf("prompt has no %s evidence: %s", probe, body)
		return "E0"
	}
	return m[len(m)-1][1]
}
