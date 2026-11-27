package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// sre-09: the Cases page asks the fleet list for each visible case's
// newest investigation by id. A case whose investigation is older than
// the database's newest 100 still gets it, and two databases holding the
// same case id each get their own.

// sreCaseInstance registers a database whose investigator concluded one
// investigation of caseID, then holds `newer` newer investigations of
// other cases. It returns that investigation.
func sreCaseInstance(t *testing.T, mgr *fleet.DatabaseManager, name, caseID string,
	newer int) sre.Investigation {
	t.Helper()
	pool := surfacePool(t)
	ctx := context.Background()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: lockChainRunner{}, Config: sre.DefaultCoordinatorConfig(
			fmt.Sprintf("api-cases:%s:%d", name, time.Now().UnixNano()))})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: caseID, Kind: sre.TriggerLock,
		Subject: "incident 1", IdempotencyKey: "case:" + name})
	if err != nil || coord.Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_investigations (deployment_id,
		database_id, id, source_case_id, trigger_kind, trigger_fingerprint, state,
		created_at, updated_at, expires_at)
		SELECT $1::uuid, $2::uuid, gen_random_uuid(), 'case:newer:' || g,
		       'lock_blocking', sha256(('newer-' || $3 || g)::bytea), 'concluded',
		       now() + g * interval '1 second', now(), now() + interval '1 day'
		FROM generate_series(1, $4::int) g`, string(scope.DeploymentID),
		string(scope.DatabaseID), name, newer); err != nil {
		t.Fatalf("seed newer investigations: %v", err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: sre.NewService(name, coord, st)})
	return inv
}

type fleetListBody struct {
	Items []struct {
		ID       string `json:"id"`
		CaseID   string `json:"case_id"`
		Database string `json:"database"`
	} `json:"items"`
	Unavailable []string `json:"unavailable"`
}

func fleetList(t *testing.T, mgr *fleet.DatabaseManager, q url.Values) fleetListBody {
	t.Helper()
	h := sreRouter(t, mgr, testViewerUser())
	code, body, _ := sreCall(t, h, "GET", "/api/v1/investigations?"+q.Encode())
	var out fleetListBody
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil {
		t.Fatalf("fleet list %s = %d %s", q.Encode(), code, body)
	}
	return out
}

func TestSREAPI_FleetListLatestPerCase(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	orders := sreCaseInstance(t, mgr, "orders", "slo:checkout", 101)
	billing := sreCaseInstance(t, mgr, "billing", "slo:checkout", 0)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "reporting",
		Status: &fleet.InstanceStatus{}})

	newest := fleetList(t, mgr, url.Values{"database": {"orders"}})
	for _, it := range newest.Items {
		if it.ID == string(orders.ID) {
			t.Fatalf("the newest-100 list carries the old investigation; the test " +
				"does not reproduce the busy database")
		}
	}

	got := fleetList(t, mgr, url.Values{"database": {"all"},
		"case_id": {"slo:checkout", "case:newer:3", "case:missing"}})
	byDB := map[string]string{}
	for _, it := range got.Items {
		byDB[it.Database+"|"+it.CaseID] = it.ID
	}
	if len(got.Items) != 3 || byDB["orders|slo:checkout"] != string(orders.ID) ||
		byDB["billing|slo:checkout"] != string(billing.ID) ||
		byDB["orders|case:newer:3"] == "" {
		t.Fatalf("latest per case = %+v, want each database's own slo:checkout "+
			"and orders' case:newer:3", got.Items)
	}
	if len(got.Unavailable) != 1 || got.Unavailable[0] != "reporting" {
		t.Fatalf("unavailable = %v, want [reporting]", got.Unavailable)
	}

	one := fleetList(t, mgr, url.Values{"database": {"orders"},
		"case_id": {"slo:checkout"}, "limit": {"5"}})
	if len(one.Items) != 1 || one.Items[0].ID != string(orders.ID) {
		t.Fatalf("single case_id = %+v, want its one investigation", one.Items)
	}
}
