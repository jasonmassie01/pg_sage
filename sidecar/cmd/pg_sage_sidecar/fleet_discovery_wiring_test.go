package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet/discovery"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Enrolment goes through the API create lifecycle: the member is
// registered with a runtime at observation and approval; Disable detaches
// it and leaves the row disabled.
func TestMetaDiscoveryMembers_EnrolAndDisable(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	member, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	extra := testdb.CreateDatabase(t, "disc_wiring")
	dbname := extra[strings.LastIndex(extra, "/")+1:]
	if i := strings.Index(dbname, "?"); i >= 0 {
		dbname = dbname[:i]
	}
	row := store.DiscoveredRow{ID: 41, Source: "cluster", DBName: dbname, Owner: "team"}
	members := metaDiscoveryMembers{mgr: fleetMgr, state: state}
	exp := time.Now().Add(time.Hour)
	id, err := members.Enrol(ctx, input.Name, row, exp)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	inst := fleetMgr.GetInstance(discovery.MemberName(row))
	assertManagedRuntime(t, inst, id, discovery.MemberName(row))
	if inst.Config.TrustLevel != "observation" || inst.Executor.ExecutionMode() != "approval" {
		t.Fatalf("enrolled runtime trust=%q mode=%q", inst.Config.TrustLevel,
			inst.Executor.ExecutionMode())
	}
	rec, _ := state.Store.Get(ctx, id)
	if rec.DiscoveredSource != "cluster" || rec.Owner != "team" || rec.ExpiresAt == nil ||
		rec.Username != input.Username || rec.DatabaseName != dbname {
		t.Fatalf("enrolled record = %+v", rec)
	}
	if err := members.Disable(ctx, id); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if fleetMgr.GetInstance(discovery.MemberName(row)) != nil {
		t.Fatal("disabled member still registered")
	}
	if rec, _ = state.Store.Get(ctx, id); rec.Enabled {
		t.Fatal("disabled member still enabled")
	}
	if fleetMgr.GetInstance(member.Name) == nil {
		t.Fatal("disabling a discovered member detached the credential member")
	}
	if err := members.Disable(ctx, member.ID); err == nil {
		t.Fatal("Disable accepted a manual member")
	}
}

// Error propagation: a failed health check rolls the row back and names
// the cause; an unknown credential member is refused.
func TestMetaDiscoveryMembers_EnrolFailures(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	if _, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}
	members := metaDiscoveryMembers{mgr: fleetMgr, state: state}
	row := store.DiscoveredRow{ID: 42, Source: "cluster", DBName: "no_such_db_x",
		Owner: "team"}
	if _, err := members.Enrol(ctx, "nobody", row, time.Now().Add(time.Hour)); err == nil ||
		!strings.Contains(err.Error(), "nobody") {
		t.Fatalf("unknown credential member: %v", err)
	}
	if _, err := members.Enrol(ctx, input.Name, row, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("enrolled a database that does not exist")
	}
	if names := registeredDatabases(t, state); len(names) != 1 {
		t.Fatalf("a failed enrolment left store rows: %v", names)
	}
}

func TestMetaDiscoverySourceDeps_ClusterPoolNeedsAConnectedMember(t *testing.T) {
	prepareMetaGlobals(t)
	deps := metaDiscoverySourceDeps(&metaDBState{})
	if _, err := deps.ClusterPool("absent"); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Fatalf("ClusterPool for a missing member: %v", err)
	}
}
