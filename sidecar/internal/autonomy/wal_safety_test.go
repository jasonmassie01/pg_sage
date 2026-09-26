package autonomy

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"testing"
)

// Regression tests for G4-B05: the WAL backstop must never set
// max_slot_wal_keep_size below the WAL a slot already retains plus headroom,
// must not bound registered consumers, and must escalate instead of
// proposing an impossible bound.

var keepSizePattern = regexp.MustCompile(`max_slot_wal_keep_size = '([0-9]+)MB'`)

func proposedKeepBytes(t *testing.T, sql string) int64 {
	t.Helper()
	match := keepSizePattern.FindStringSubmatch(sql)
	if len(match) != 2 {
		t.Fatalf("no keep size in %q", sql)
	}
	megabytes, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		t.Fatalf("parse keep size: %v", err)
	}
	return megabytes << 20
}

func TestWALBoundTargetKeepsHeadroomAboveRetained(t *testing.T) {
	retained := int64(14) << 30
	target := walBoundTarget(retained, defaultWALBackstopBytes)
	if target < retained*3/2 {
		t.Fatalf("target = %d, want >= 1.5 x retained (%d)", target, retained*3/2)
	}
	if got := walBoundTarget(1<<20, defaultWALBackstopBytes); got != defaultWALBackstopBytes {
		t.Fatalf("small retention target = %d, want configured limit", got)
	}
}

func TestWALBoundNeverInvalidatesLaggingSlot(t *testing.T) {
	pool := requireAutonomyDB(t)
	custodian := NewPostgresWALCustodian(pool, "testdb", PostgresWALOptions{})
	retained := int64(14) << 30

	proposal, err := custodian.scanSlot(context.Background(), adapterRow{values: []any{
		"lagging_cdc", "logical", true, retained,
	}}, 0, false)

	if err != nil {
		t.Fatalf("scanSlot: %v", err)
	}
	if got := proposedKeepBytes(t, proposal.SQL); got <= retained {
		t.Fatalf("proposed keep size %d would invalidate slot retaining %d", got, retained)
	}
}

func TestWALBoundEscalatesForRegisteredConsumer(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	slot := fmt.Sprintf("registered_slot_%d", 7)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.slot_consumer_registry
		(slot_name, owner_tag, registered) VALUES ($1, 'debezium', true)
		ON CONFLICT (slot_name) DO UPDATE SET registered = true`, slot); err != nil {
		t.Fatalf("register consumer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.slot_consumer_registry WHERE slot_name=$1", slot)
	})
	custodian := NewPostgresWALCustodian(pool, "testdb", PostgresWALOptions{})

	proposal, err := custodian.scanSlot(ctx, adapterRow{values: []any{
		slot, "logical", true, int64(14) << 30,
	}}, 0, false)

	if err != nil {
		t.Fatalf("scanSlot: %v", err)
	}
	if proposal.SQL != "" || proposal.Plan == "" {
		t.Fatalf("registered consumer proposal = %#v, want escalation plan only", proposal)
	}
}

func TestWALBoundEscalatesWhenDiskCannotHoldHeadroom(t *testing.T) {
	pool := requireAutonomyDB(t)
	custodian := NewPostgresWALCustodian(pool, "testdb", PostgresWALOptions{})
	retained := int64(14) << 30

	proposal, err := custodian.scanSlot(context.Background(), adapterRow{values: []any{
		"disk_bound_slot", "physical", true, retained,
	}}, int64(20)<<30, true)

	if err != nil {
		t.Fatalf("scanSlot: %v", err)
	}
	if proposal.SQL != "" || proposal.Plan == "" {
		t.Fatalf("impossible bound proposal = %#v, want escalation plan only", proposal)
	}
	if proposal.Evidence["retained_wal_bytes"] != retained {
		t.Fatalf("escalation evidence = %#v", proposal.Evidence)
	}
}
