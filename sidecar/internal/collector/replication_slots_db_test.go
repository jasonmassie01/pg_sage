package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// No concurrent-access tests: collectReplication runs on the collector's
// own goroutine once per cycle and shares nothing.

// The slot columns that differ by major version are read by name through
// to_jsonb, so one statement runs on every supported server (CHECK-12).
func TestReplicationSlotsSQL_ReadsVersionColumnsByName(t *testing.T) {
	for _, col := range []string{
		"wal_status", "safe_wal_size", "temporary",
		"inactive_since", "synced", "invalidation_reason",
	} {
		if !strings.Contains(replicationSlotsSQL, "to_jsonb(s) ->> '"+col+"'") {
			t.Errorf("replicationSlotsSQL does not read %s by name", col)
		}
	}
	if strings.Contains(replicationSlotsSQL, "server_version") {
		t.Error("replicationSlotsSQL must not branch on the server version")
	}
	if !strings.Contains(replicationReplicasSQL, "application_name") {
		t.Error("replicationReplicasSQL must read application_name")
	}
}

func slotServer(t *testing.T) (int, func(string)) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	var version int
	var walLevel string
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
		current_setting('wal_level')`).Scan(&version, &walLevel); err != nil {
		t.Fatalf("read server version and wal_level: %v", err)
	}
	if walLevel != "logical" {
		t.Skipf("wal_level=%s: logical slots need wal_level=logical", walLevel)
	}
	create := func(name string) {
		if _, err := pool.Exec(ctx,
			`SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name); err != nil {
			t.Fatalf("create slot %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`SELECT pg_drop_replication_slot($1)`, name)
		})
	}
	return version, create
}

func TestCollectReplication_SlotColumnsOnServer(t *testing.T) {
	version, create := slotServer(t)
	pool := testPool(t)
	ctx := context.Background()
	name := fmt.Sprintf("rules09_slot_%d", time.Now().UnixNano())
	create(name)
	tmp := name + "_tmp"
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx,
		`SELECT pg_create_logical_replication_slot($1, 'pgoutput', true)`, tmp); err != nil {
		t.Fatalf("create temporary slot: %v", err)
	}
	before := time.Now().Add(-time.Minute)

	rep, err := New(pool, testConfig(), version, noopLog).collectReplication(ctx)
	if err != nil || rep == nil {
		t.Fatalf("collectReplication = %v, %v; want slots", rep, err)
	}
	slots := map[string]SlotInfo{}
	for _, s := range rep.Slots {
		slots[s.SlotName] = s
	}
	s, ok := slots[name]
	if !ok {
		t.Fatalf("slot %s not collected: %+v", name, rep.Slots)
	}
	if s.Active || s.Temporary || s.Synced || s.SlotType != "logical" {
		t.Fatalf("slot = %+v, want inactive, permanent, unsynced logical", s)
	}
	if s.WALStatus == nil || *s.WALStatus != "reserved" {
		t.Fatalf("wal_status = %v, want reserved", s.WALStatus)
	}
	if s.SafeWALSize != nil || s.InvalidationReason != nil {
		t.Fatalf("safe_wal_size=%v invalidation_reason=%v, want NULL (no keep size, valid)",
			s.SafeWALSize, s.InvalidationReason)
	}
	assertInactiveSince(t, version, s, before)
	if ts, ok := slots[tmp]; !ok || !ts.Temporary {
		t.Fatalf("temporary slot = %+v (found %v), want Temporary", ts, ok)
	}
}

func assertInactiveSince(t *testing.T, version int, s SlotInfo, before time.Time) {
	t.Helper()
	if version < 170000 {
		if s.InactiveSince != nil {
			t.Fatalf("PG %d has no inactive_since, got %v", version, s.InactiveSince)
		}
		return
	}
	if s.InactiveSince == nil || s.InactiveSince.Before(before) ||
		s.InactiveSince.After(time.Now().Add(time.Minute)) {
		t.Fatalf("inactive_since = %v, want about now", s.InactiveSince)
	}
}

func TestCollectReplication_ReplicasReadApplicationName(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, replicationReplicasSQL)
	if err != nil {
		t.Fatalf("replicas query: %v", err)
	}
	fd := rows.FieldDescriptions()
	if _, err := scanReplicaRows(rows); err != nil {
		t.Fatalf("scan replicas: %v", err)
	}
	if len(fd) != 11 || fd[0].Name != "application_name" {
		t.Fatalf("replica columns = %d, first %v; want 11 starting with application_name",
			len(fd), fd)
	}
}
