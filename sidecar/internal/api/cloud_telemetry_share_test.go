package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// clouds-08 CHECK-03: three databases on one instance share one runtime,
// and GET /api/v1/cloud-telemetry lists the instance once with all three;
// a member's ?database= still finds it. No database: a nil manager
// accepts every database name.
func TestCloudTelemetryShareListsTheInstanceOnce(t *testing.T) {
	rt := cloudtel.Unavailable("share-api-a", "rds", "no AWS credentials")
	members := []string{"share-api-a", "share-api-b", "share-api-c"}
	for _, db := range members {
		cloudtel.RegisterMember(db, rt)
	}
	t.Cleanup(func() {
		for _, db := range members {
			cloudtel.UnregisterMember(db, rt)
		}
	})
	code, body := serveManaged(t, cloudTelemetryHandler(nil), "GET",
		"/api/v1/cloud-telemetry", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK {
		t.Fatalf("status = %d %v", code, body)
	}
	var entries []map[string]any
	for _, raw := range dbs {
		st, _ := raw.(map[string]any)
		if fmt.Sprint(st["database"]) == "share-api-a" ||
			fmt.Sprint(st["database"]) == "share-api-b" {
			entries = append(entries, st)
		}
	}
	if len(entries) != 1 || fmt.Sprint(entries[0]["databases"]) !=
		"[share-api-a share-api-b share-api-c]" {
		t.Fatalf("instance entries = %v, want one with three databases", entries)
	}
	code, body = serveManaged(t, cloudTelemetryHandler(nil), "GET",
		"/api/v1/cloud-telemetry?database=share-api-c", nil, 0)
	dbs, _ = body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 {
		t.Fatalf("member filter = %d %v, want the shared entry", code, body)
	}
	if st, _ := dbs[0].(map[string]any); st["database"] != "share-api-a" {
		t.Fatalf("member filter returned %v", st)
	}
}

// GET /api/v1/managed-changes names the databases that share the
// instance, so an operator sees a change held by a peer database.
func TestManagedChangesPeerMemberDatabases(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanManagedChanges(t, pool)
	mgr := phase2MgrWithPool(pool)
	t.Cleanup(managedparam.JoinPeers("rds|peer-api", "testdb", managedparam.NewStore(pool)))
	t.Cleanup(managedparam.JoinPeers("rds|peer-api", "peer-api-b",
		managedparam.NewStore(pool)))
	code, body := serveManaged(t, managedChangesHandler(mgr), "GET",
		"/api/v1/managed-changes?database=testdb", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 {
		t.Fatalf("list = %d %v", code, body)
	}
	entry, _ := dbs[0].(map[string]any)
	raw, _ := entry["member_databases"].([]any)
	var got []string
	for _, m := range raw {
		got = append(got, fmt.Sprint(m))
	}
	if !slices.Equal(got, []string{"peer-api-b", "testdb"}) {
		t.Fatalf("member_databases = %v, want [peer-api-b testdb]", entry["member_databases"])
	}
}
