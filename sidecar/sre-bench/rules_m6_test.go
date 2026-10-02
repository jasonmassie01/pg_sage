package srebench

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Rules-only baselines for the M6 families: naive readings of the same
// evidence (any requested checkpoint means max_wal_size, any temp usage
// means a runaway, any lag means replay, the most waited LWLock wins).

func ckptAt(at time.Duration, req int64) probes.Result {
	return probes.Result{ProbeID: probes.CheckpointActivity, Status: probes.StatusOK,
		ObservedAt: t0.Add(at), Rows: []probes.Row{{"requested_checkpoints": req,
			"timed_checkpoints": int64(1), "wal_bytes": int64(0)}}}
}

func TestRulesOnly_Checkpoint(t *testing.T) {
	cases := []struct {
		name string
		ev   []probes.Result
		want string
	}{
		{"requested", []probes.Result{ckptAt(9*time.Second, 12), ckptAt(0, 10)},
			"max_wal_size_undersized"},
		{"none requested", []probes.Result{ckptAt(0, 10), ckptAt(9*time.Second, 10)}, ""},
		{"one sample", []probes.Result{ckptAt(0, 10)}, ""},
	}
	for _, c := range cases {
		if o := derive(sre.TriggerCheckpoint, "", c.ev...); o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
}

func tempAt(at time.Duration, bytes int64) probes.Result {
	return probes.Result{ProbeID: probes.TempFileActivity, Status: probes.StatusOK,
		ObservedAt: t0.Add(at), Rows: []probes.Row{{"temp_files": int64(1),
			"temp_bytes": bytes}}}
}

func holdersAt(bytes ...int64) probes.Result {
	r := probes.Result{ProbeID: probes.TempFileHolders, Status: probes.StatusEmpty,
		ObservedAt: t0}
	for i, b := range bytes {
		r.Status = probes.StatusOK
		r.Rows = append(r.Rows, probes.Row{"pid": int64(100 + i), "bytes": b,
			"in_current_database": true, "files": int64(1)})
	}
	return r
}

func spillAt(at time.Duration, blks int64) probes.Result {
	return probes.Result{ProbeID: probes.TempSpillStatements, Status: probes.StatusOK,
		ObservedAt: t0.Add(at), Rows: []probes.Row{{"queryid": int64(5), "calls": int64(1),
			"temp_blks_written": blks, "block_size": int64(8192)}}}
}

func TestRulesOnly_TempFiles(t *testing.T) {
	cases := []struct {
		name string
		ev   []probes.Result
		want string
	}{
		{"live holder", []probes.Result{holdersAt(1 << 20)}, "runaway_spill_query"},
		{"statement spilled", []probes.Result{holdersAt(), spillAt(0, 10),
			spillAt(9*time.Second, 20)}, "repeated_spill_statement"},
		{"large historic counter", []probes.Result{holdersAt(), tempAt(0, 1<<30),
			tempAt(9*time.Second, 1<<30)}, "runaway_spill_query"},
		{"nothing", []probes.Result{holdersAt(), tempAt(0, 0), tempAt(9*time.Second, 0)}, ""},
	}
	for _, c := range cases {
		if o := derive(sre.TriggerTempFiles, "", c.ev...); o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
}

func lagAt(total int64) probes.Result {
	r := probes.Result{ProbeID: probes.ReplicationLag, Status: probes.StatusEmpty,
		ObservedAt: t0}
	if total >= 0 {
		r.Status = probes.StatusOK
		r.Rows = []probes.Row{{"pid": int64(1), "replay_lag_bytes": total,
			"send_backlog_bytes": total, "flush_backlog_bytes": int64(0),
			"replay_backlog_bytes": int64(0)}}
	}
	return r
}

func TestRulesOnly_ReplicationLag(t *testing.T) {
	if o := derive(sre.TriggerReplicationLag, "", lagAt(32<<20)); o.Root !=
		"standby_replay_backlog" {
		t.Errorf("lagging replica: root %q", o.Root)
	}
	for _, total := range []int64{1 << 20, -1} {
		if o := derive(sre.TriggerReplicationLag, "", lagAt(total)); o.Root != "" {
			t.Errorf("total %d: root %q", total, o.Root)
		}
	}
}

func lwAt(at time.Duration, event string, n int64) probes.Result {
	return probes.Result{ProbeID: probes.LWLockWaits, Status: probes.StatusOK,
		ObservedAt: t0.Add(at), Rows: []probes.Row{{"wait_event_type": "LWLock",
			"wait_event": event, "backends": n, "active_backends": n}}}
}

func TestRulesOnly_LWLock(t *testing.T) {
	o := derive(sre.TriggerLWLock, "", lwAt(0, "WALWrite", 30),
		lwAt(3*time.Second, "LockManager", 1))
	if o.Root != "lock_manager_contention" {
		t.Errorf("last sample's LWLock: root %q", o.Root)
	}
	if o := derive(sre.TriggerLWLock, "", lwAt(0, "ProcArray", 9)); o.Root != "" {
		t.Errorf("unmodeled LWLock: root %q", o.Root)
	}
}
