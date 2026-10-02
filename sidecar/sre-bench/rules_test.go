package srebench

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The rules-only baseline is a first-match rule list per family over the
// same evidence the causal graph read: no contradictions, no per-subject
// attribution, no amplification. It shows what the graph adds.

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func edge(blockerKind, blockerState, lockType, mode string) probes.Row {
	return probes.Row{"waiter_pid": int64(200), "blocker_pid": int64(100),
		"blocker_kind": blockerKind, "blocker_state": blockerState, "lock_type": lockType,
		"requested_mode": mode, "relation": "bench_t"}
}

func lockGraph(status probes.Status, rows ...probes.Row) probes.Result {
	return probes.Result{ProbeID: probes.LockGraph, Status: status, ObservedAt: t0,
		Rows: rows}
}

func derive(fam sre.TriggerKind, subject string, ev ...probes.Result) Outcome {
	sc := Scenario{ID: "r", Family: fam, Subject: subject}
	tr := Trace{Outcome: Outcome{ProbeCount: len(ev), Measured: true,
		FirstEvidence: time.Millisecond, Packet: time.Second}, Evidence: ev}
	return RulesOnly{}.Derive(sc, tr)
}

func TestRulesOnly_Lock(t *testing.T) {
	idleRow := edge("backend", "idle in transaction", "transactionid", "ShareLock")
	cases := []struct {
		name string
		ev   probes.Result
		want string
	}{
		{"no waits", lockGraph(probes.StatusEmpty), ""},
		{"lock graph failed", lockGraph(probes.StatusError), ""},
		{"prepared blocker", lockGraph(probes.StatusOK, edge("prepared_xact", "",
			"transactionid", "ShareLock")), "prepared_xact_holder"},
		{"DDL waiter behind an idle holder", lockGraph(probes.StatusOK, idleRow,
			edge("backend", "idle in transaction", "relation", "AccessExclusiveLock")),
			"ddl_lock_queue"},
		{"idle holder", lockGraph(probes.StatusOK, idleRow), "idle_in_tx_holder"},
		{"active holder", lockGraph(probes.StatusOK, edge("backend", "active",
			"transactionid", "ShareLock")), "hot_row_contention"},
	}
	for _, c := range cases {
		o := derive(sre.TriggerLock, "", c.ev)
		if o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
}

func connRows(idleByApp map[string]int64, waiting int64) []probes.Row {
	var rows []probes.Row
	for app, n := range idleByApp {
		rows = append(rows, probes.Row{"in_current_database": true, "application_name": app,
			"state": "idle", "backends": n, "waiting_on_lock": int64(0)})
	}
	if waiting > 0 {
		rows = append(rows, probes.Row{"in_current_database": true, "application_name": "q",
			"state": "active", "backends": waiting, "waiting_on_lock": waiting})
	}
	// Another database's pool is never this database's pressure.
	rows = append(rows, probes.Row{"in_current_database": false, "application_name": "other",
		"state": "idle", "backends": int64(40), "waiting_on_lock": int64(9)})
	return rows
}

func conn(at time.Duration, rows []probes.Row) probes.Result {
	return probes.Result{ProbeID: probes.ConnectionSaturation, Status: probes.StatusOK,
		ObservedAt: t0.Add(at), Rows: rows}
}

func TestRulesOnly_Connections(t *testing.T) {
	cases := []struct {
		name          string
		first, second []probes.Row
		want          string
	}{
		{"lock waits", connRows(map[string]int64{"a": 2}, 3),
			connRows(map[string]int64{"a": 2}, 4), "blocked_backlog"},
		{"warm-up growth", connRows(map[string]int64{"a": 5}, 0),
			connRows(map[string]int64{"a": 7}, 0), "connection_leak"},
		{"bounded pools add up", connRows(map[string]int64{"a": 6, "b": 6}, 0),
			connRows(map[string]int64{"a": 6, "b": 6}, 0), "pool_fan_out"},
		{"quiet", connRows(map[string]int64{"a": 4}, 0),
			connRows(map[string]int64{"a": 4}, 0), ""},
	}
	for _, c := range cases {
		// Evidence order is not time order: the rule compares by observed_at.
		o := derive(sre.TriggerConnections, "", conn(3*time.Second, c.second),
			conn(0, c.first))
		if o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
	if o := derive(sre.TriggerConnections, ""); o.Root != "" || o.ProbeCount != 0 {
		t.Fatalf("no evidence: %+v", o)
	}
}

func slots(rows ...probes.Row) probes.Result {
	return probes.Result{ProbeID: probes.ReplicationSlots, Status: probes.StatusOK,
		ObservedAt: t0, Rows: rows}
}

func walAt(at time.Duration, bytes float64) probes.Result {
	return probes.Result{ProbeID: probes.WALCheckpoint, Status: probes.StatusOK,
		ObservedAt: t0.Add(at), Rows: []probes.Row{{"wal_bytes": bytes}}}
}

func archiver(failedAfterSuccess bool) probes.Result {
	row := probes.Row{"archive_mode": "on", "last_archived_time": t0.Add(-time.Minute),
		"last_failed_time": t0.Add(-2 * time.Minute)}
	if failedAfterSuccess {
		row["last_failed_time"] = t0.Add(-time.Second)
	}
	return probes.Result{ProbeID: probes.Archiver, Status: probes.StatusOK, ObservedAt: t0,
		Rows: []probes.Row{row}}
}

func TestRulesOnly_WAL(t *testing.T) {
	inactive := probes.Row{"slot_name": "s", "active": false, "retained_bytes": 1.0}
	active := probes.Row{"slot_name": "s", "active": true, "retained_bytes": 1.0}
	quiet := []probes.Result{walAt(0, 1e6), walAt(3*time.Second, 1.1e6)}
	surge := []probes.Result{walAt(0, 1e6), walAt(3*time.Second, 1e6+64<<20)}
	cases := []struct {
		name string
		ev   []probes.Result
		want string
	}{
		{"inactive slot", append([]probes.Result{slots(inactive), archiver(false)}, quiet...),
			"inactive_slot"},
		{"any active slot", append([]probes.Result{slots(active), archiver(false)}, quiet...),
			"slow_consumer"},
		{"failing archiver", append([]probes.Result{slots(), archiver(true)}, quiet...),
			"archiver_failure"},
		{"surge", append([]probes.Result{slots(), archiver(false)}, surge...), "write_surge"},
		{"quiet", append([]probes.Result{slots(), archiver(false)}, quiet...), ""},
		{"one WAL sample", []probes.Result{slots(), walAt(0, 1e9)}, ""},
	}
	for _, c := range cases {
		if o := derive(sre.TriggerWAL, "", c.ev...); o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
}

func shift(qid int64, flipped bool, before, after float64) probes.Row {
	return probes.Row{"queryid": qid, "plan_flipped": flipped, "before_calls": int64(20),
		"before_mean_ms": before, "after_calls": int64(20), "after_mean_ms": after,
		"previous_plan_hash": "v1:a", "current_plan_hash": "v1:b"}
}

func plans(rows ...probes.Row) probes.Result {
	return probes.Result{ProbeID: probes.PlanRegressions, Status: probes.StatusOK,
		ObservedAt: t0, Rows: rows}
}

func TestRulesOnly_Plan(t *testing.T) {
	cases := []struct {
		name string
		ev   probes.Result
		want string
	}{
		{"flip without slowdown", plans(shift(7, true, 1, 1.05)), "plan_flip_regression"},
		{"same plan slowdown", plans(shift(7, false, 1, 2)), "same_plan_latency_regression"},
		{"steady", plans(shift(7, false, 1, 1.1)), ""},
		{"another query regressed", plans(shift(8, true, 1, 9), shift(7, false, 1, 1)), ""},
		{"query absent", plans(shift(8, false, 1, 9)), ""},
	}
	for _, c := range cases {
		if o := derive(sre.TriggerPlan, "queryid 7", c.ev); o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
}

// Stored evidence is JSON: numbers are json.Number and times strings.
func TestRulesOnly_ReadsStoredEvidence(t *testing.T) {
	raw, err := json.Marshal(lockGraph(probes.StatusOK, edge("backend",
		"idle in transaction", "tuple", "ExclusiveLock")))
	if err != nil {
		t.Fatal(err)
	}
	res, err := decodeEvidence(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := res.Rows[0]["waiter_pid"].(json.Number); !ok {
		t.Fatalf("waiter_pid decoded as %T, want json.Number", res.Rows[0]["waiter_pid"])
	}
	o := derive(sre.TriggerLock, "", res)
	if o.Root != "idle_in_tx_holder" || o.State != sre.StateConcluded ||
		len(o.Ranked) != 1 || o.Ranked[0] != o.Root || o.ProbeCount != 1 || !o.Measured ||
		o.Packet != time.Second {
		t.Fatalf("outcome %+v", o)
	}
	if _, err := decodeEvidence([]byte("{not json")); err == nil {
		t.Fatal("garbage evidence decoded")
	}
}

func TestRulesOnly_AbstentionShape(t *testing.T) {
	o := derive(sre.TriggerLock, "", lockGraph(probes.StatusEmpty))
	if o.State != sre.StateInconclusive || o.Root != "" || len(o.Ranked) != 0 {
		t.Fatalf("outcome %+v", o)
	}
	if o := derive(sre.TriggerOperator, ""); o.Root != "" {
		t.Fatalf("unknown family got root %q", o.Root)
	}
}
