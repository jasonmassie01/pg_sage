package probes

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Action probes (Sage SRE M5) are a separate, fixed registry: the model
// can never pick them (they are not in the diagnostic catalog), and they
// never return query text or application names, only hashes.

func TestActionRegistryIsSeparateFromTheCatalog(t *testing.T) {
	reg := ActionRegistry()
	for _, id := range []ID{SignalTarget, RecoverySample} {
		if _, ok := Catalog().Spec(id); ok {
			t.Fatalf("%s is in the diagnostic catalog", id)
		}
		s, ok := reg.Spec(id)
		if !ok {
			t.Fatalf("%s missing from the action registry", id)
		}
		if s.Args != ArgsBackend || s.Version != "v1" || s.Family != FamilyLocks ||
			s.StatementTimeout > MaxStatementTimeout || s.MaxRows > MaxRows {
			t.Fatalf("%s spec = %+v", id, s)
		}
	}
	if got := len(reg.IDs()); got != 2 {
		t.Fatalf("action registry has %d probes, want 2", got)
	}
}

func TestActionProbesReturnNoQueryTextOrApplicationNames(t *testing.T) {
	for _, id := range []ID{SignalTarget, RecoverySample} {
		s, _ := ActionRegistry().Spec(id)
		sql := strings.ToLower(s.Variants[0].SQL)
		for _, leak := range []string{"a.query as", "a.query,", ", a.query\n",
			"a.application_name as", "a.application_name,", "client_addr"} {
			if strings.Contains(sql, leak) {
				t.Fatalf("%s returns raw text (%q)", id, leak)
			}
		}
		if !strings.Contains(sql, "current_database()") {
			t.Fatalf("%s is not scoped to the current database", id)
		}
	}
	s, _ := ActionRegistry().Spec(SignalTarget)
	sql := s.Variants[0].SQL
	for _, want := range []string{"sha256(", "a.backend_start = $3", "a.pid = $2",
		"pg_blocking_pids", "pg_is_in_recovery()", "rolreplication"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("signal_target SQL lacks %q", want)
		}
	}
}

func targetRow() Row {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return Row{"pid": json.Number("5151"), "backend_start": start.Format(time.RFC3339Nano),
		"query_start": start.Add(time.Minute), "xact_start": start.Add(time.Minute),
		"datname": "orders", "usename": "app", "state": "active",
		"backend_type": "client backend", "waiting": false,
		"query_id": json.Number("-9007199254740993"), "query_hash": strings.Repeat("a", 64),
		"blocking": int64(2), "in_recovery": false, "in_current_database": true,
		"privileged_role": false, "protected_application": false,
		"application_hash": strings.Repeat("b", 64)}
}

func TestSignalTargetsDecodes(t *testing.T) {
	res := Result{ProbeID: SignalTarget, Status: StatusOK, Rows: []Row{targetRow()}}
	got, err := SignalTargets(res)
	if err != nil || len(got) != 1 {
		t.Fatalf("SignalTargets = %+v, %v", got, err)
	}
	r := got[0]
	if r.PID != 5151 || r.QueryID != -9007199254740993 || r.Database != "orders" ||
		r.User != "app" || r.State != "active" || r.Blocking != 2 ||
		!r.InCurrentDatabase || r.InRecovery || r.QueryHash != strings.Repeat("a", 64) ||
		r.BackendStart.IsZero() || r.QueryStart.IsZero() || r.XactStart.IsZero() ||
		r.BackendType != "client backend" || r.ApplicationHash == "" {
		t.Fatalf("decoded target = %+v", r)
	}
}

func TestSignalTargetsTypedFailures(t *testing.T) {
	if _, err := SignalTargets(Result{ProbeID: LockGraph, Status: StatusOK}); err == nil {
		t.Fatal("a lock_graph result decoded as signal_target")
	}
	_, err := SignalTargets(Result{ProbeID: SignalTarget, Status: StatusNoPrivilege,
		Reason: "permission_denied"})
	var unavailable *UnavailableError
	if err == nil || !errors.As(err, &unavailable) ||
		unavailable.Status != StatusNoPrivilege {
		t.Fatalf("no_privilege = %v, want *UnavailableError", err)
	}
	got, err := SignalTargets(Result{ProbeID: SignalTarget, Status: StatusEmpty})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty = %+v, %v", got, err)
	}
	bad := targetRow()
	delete(bad, "pid")
	if _, err := SignalTargets(Result{ProbeID: SignalTarget, Status: StatusOK,
		Rows: []Row{bad}}); err == nil {
		t.Fatal("a row without pid decoded")
	}
	bad = targetRow()
	bad["backend_start"] = "yesterday"
	if _, err := SignalTargets(Result{ProbeID: SignalTarget, Status: StatusOK,
		Rows: []Row{bad}}); err == nil {
		t.Fatal("a row without a readable backend_start decoded")
	}
}

func TestRecoveryRowsDecodes(t *testing.T) {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	res := Result{ProbeID: RecoverySample, Status: StatusOK, Rows: []Row{
		{"pid": int64(5151), "backend_start": start, "state": "active",
			"waiting": false, "blocked_by_target": false, "is_target": true},
		{"pid": json.Number("20"), "backend_start": start, "state": "active",
			"waiting": true, "blocked_by_target": true, "is_target": false},
	}}
	got, err := RecoveryRows(res)
	if err != nil || len(got) != 2 || !got[0].IsTarget || got[0].PID != 5151 ||
		!got[1].Waiting || !got[1].BlockedByTarget || got[1].PID != 20 {
		t.Fatalf("RecoveryRows = %+v, %v", got, err)
	}
	if _, err := RecoveryRows(Result{ProbeID: RecoverySample,
		Status: StatusError}); err == nil {
		t.Fatal("an error result decoded as rows")
	}
}

func TestLockEdgeIdentitiesKeepWaiterAndQueryIdentity(t *testing.T) {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	res := Result{ProbeID: LockGraph, Status: StatusOK, Rows: []Row{{
		"waiter_pid": int64(20), "waiter_backend_start": start.Format(time.RFC3339Nano),
		"lock_type": "relation", "requested_mode": "AccessExclusiveLock",
		"blocker_pid": int64(5151), "blocker_kind": "backend", "blocker_state": "active",
		"blocker_backend_start": start, "blocker_query_id": json.Number("77"),
	}}}
	got, err := LockEdgeIdentities(res)
	if err != nil || len(got) != 1 {
		t.Fatalf("LockEdgeIdentities = %+v, %v", got, err)
	}
	e := got[0]
	if e.WaiterPID != 20 || !e.WaiterBackendStart.Equal(start) || e.BlockerQueryID != 77 ||
		e.BlockerPID != 5151 || !e.BlockerBackendStart.Equal(start) {
		t.Fatalf("edge identity = %+v", e)
	}
}
