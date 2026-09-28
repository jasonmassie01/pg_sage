package probes

import (
	"errors"
	"math"
	"testing"
	"time"
)

// M2 decoders: connection groups, replication slots, WAL volume, the
// archiver and pg_sage's own recent actions. Unknown numbers stay NaN;
// unavailable results are errors, never a healthy empty answer.

func TestConnectionGroups_Decodes(t *testing.T) {
	started := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	res := okResult(ConnectionSaturation, nil, Row{"in_current_database": true,
		"application_name": "orders", "client_addr": "10.0.0.7", "state": "idle",
		"backends": int64(40), "waiting_on_lock": int64(0), "max_connections": int64(100),
		"reserved_connections": int64(3), "total_client_backends": int64(52),
		"server_started_at": started})
	gs, err := ConnectionGroups(res)
	if err != nil || len(gs) != 1 {
		t.Fatalf("groups = %+v (%v)", gs, err)
	}
	g := gs[0]
	if !g.InCurrentDatabase || g.Application != "orders" || g.State != "idle" ||
		g.Backends != 40 || g.MaxConnections != 100 || g.ReservedConnections != 3 ||
		g.TotalClientBackends != 52 || !g.ServerStartedAt.Equal(started) {
		t.Fatalf("group = %+v", g)
	}
	if _, err := ConnectionGroups(okResult(ConnectionSaturation, nil,
		Row{"state": "idle"})); err == nil {
		t.Fatal("a row without backends decoded")
	}
}

func TestSlots_DecodesUnknownRetentionAsNaN(t *testing.T) {
	since := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	res := okResult(ReplicationSlots, nil,
		Row{"slot_name": "cdc", "slot_type": "logical", "active": false,
			"wal_status": "extended", "retained_bytes": int64(1 << 30),
			"safe_wal_size": nil, "database": "orders", "inactive_since": since},
		Row{"slot_name": "lost", "slot_type": "physical", "active": false,
			"wal_status": "lost", "retained_bytes": nil})
	slots, err := Slots(res)
	if err != nil || len(slots) != 2 {
		t.Fatalf("slots = %+v (%v)", slots, err)
	}
	if slots[0].Name != "cdc" || slots[0].Active || slots[0].RetainedBytes != 1<<30 ||
		!math.IsNaN(slots[0].SafeWALSize) || !slots[0].InactiveSince.Equal(since) {
		t.Fatalf("slot 0 = %+v", slots[0])
	}
	if !math.IsNaN(slots[1].RetainedBytes) {
		t.Fatalf("unknown retention decoded as %v, want NaN", slots[1].RetainedBytes)
	}
}

func TestWALAndArchiverStats_Decode(t *testing.T) {
	reset := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	wal, err := WALStats(okResult(WALCheckpoint, nil, Row{"wal_bytes": 123456.0,
		"wal_stats_reset": reset}))
	if err != nil || wal.WALBytes != 123456 || !wal.StatsReset.Equal(reset) {
		t.Fatalf("wal = %+v (%v)", wal, err)
	}
	failed := reset.Add(time.Hour)
	arch, err := ArchiverStats(okResult(Archiver, nil, Row{"archive_mode": "on",
		"archived_count": int64(10), "failed_count": int64(3),
		"last_archived_time": reset, "last_failed_time": failed, "stats_reset": reset}))
	if err != nil || arch.Mode != "on" || arch.Archived != 10 || arch.Failed != 3 ||
		!arch.LastFailedAt.Equal(failed) || !arch.LastArchivedAt.Equal(reset) {
		t.Fatalf("archiver = %+v (%v)", arch, err)
	}
	if _, err := WALStats(okResult(WALCheckpoint, nil)); err == nil {
		t.Fatal("an empty wal_checkpoint result decoded as a healthy zero")
	}
}

func TestSageActionRows_Decode(t *testing.T) {
	at := time.Date(2026, 9, 27, 9, 30, 0, 0, time.UTC)
	rows, err := SageActionRows(okResult(SageActions, nil, Row{"id": int64(42),
		"action_type": "create_index", "outcome": "success", "executed_at": at,
		"age_s": 120.0}))
	if err != nil || len(rows) != 1 || rows[0].ID != 42 ||
		rows[0].ActionType != "create_index" || rows[0].AgeS != 120 ||
		!rows[0].ExecutedAt.Equal(at) {
		t.Fatalf("actions = %+v (%v)", rows, err)
	}
	if rows, err := SageActionRows(okResult(SageActions, nil)); err != nil || rows != nil {
		t.Fatalf("no recent actions = %+v (%v), want an empty observation", rows, err)
	}
}

func TestM2Decoders_RejectUnavailableAndForeignResults(t *testing.T) {
	denied := Result{ProbeID: Archiver, Status: StatusNoPrivilege, Reason: "denied"}
	var unavailable *UnavailableError
	if _, err := ArchiverStats(denied); !errors.As(err, &unavailable) {
		t.Fatalf("no_privilege archiver = %v, want UnavailableError", err)
	}
	if _, err := Slots(okResult(ConnectionSaturation, nil)); err == nil {
		t.Fatal("a connection result decoded as slots")
	}
	for name, decode := range map[string]func(Result) error{
		"groups":  func(r Result) error { _, err := ConnectionGroups(r); return err },
		"slots":   func(r Result) error { _, err := Slots(r); return err },
		"wal":     func(r Result) error { _, err := WALStats(r); return err },
		"actions": func(r Result) error { _, err := SageActionRows(r); return err },
	} {
		if err := decode(Result{Status: StatusError, Reason: "timeout"}); err == nil {
			t.Errorf("%s decoded an error result", name)
		}
	}
}
