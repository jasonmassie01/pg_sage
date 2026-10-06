package specialist

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// End to end on PostgreSQL: real MCP tokens (hashed, scoped, expiring), the
// real investigation store and coordinator (a canned lock graph stands in
// for the monitored database's catalog), the production fleet directory,
// the request store and the HTTP handler.

type lockRunner struct{}

func (lockRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
	if id == probes.LockGraph {
		res.Status = probes.StatusOK
		res.Rows = []probes.Row{{"waiter_pid": int64(20), "lock_type": "relation",
			"requested_mode": "AccessExclusiveLock", "relation": "public.orders",
			"blocker_pid": int64(4242), "blocker_kind": "backend",
			"blocker_state": "idle in transaction", "blocker_waiting": false,
			"blocker_xact_age_s":    90.0,
			"blocker_backend_start": time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)}}
	}
	return res
}

type liveFixture struct {
	pool    *pgxpool.Pool
	handler http.Handler
	coord   *sre.Coordinator
	svc     *sre.Service
	tokens  map[string]string // label -> secret
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	return newLiveFixtureWith(t, lockRunner{})
}

func newLiveFixtureWith(t *testing.T, runner sre.ProbeRunner) *liveFixture {
	t.Helper()
	pool := livePool(t)
	ctx := context.Background()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: runner,
		Config: sre.DefaultCoordinatorConfig(fmt.Sprintf("specialist:%d",
			time.Now().UnixNano()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	svc := sre.NewService("orders", coord, st)
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: svc})
	spec, err := NewService(Deps{Directory: NewFleetDirectory(mgr, nil),
		Store: NewPGStore(pool), Limits: DefaultLimits(), KeepIdentifiers: true})
	if err != nil {
		t.Fatal(err)
	}
	tokens := mcptoken.NewStore(pool)
	f := &liveFixture{pool: pool, coord: coord, svc: svc, tokens: map[string]string{},
		handler: NewHandler(spec, NewTokenAuthenticator(tokens), HandlerOptions{})}
	for label, req := range map[string]mcptoken.CreateRequest{
		"read": {Name: "Datadog", Scopes: []string{"read"}, Databases: []string{"orders"}},
		"propose": {Name: "AWS DevOps Agent", Scopes: []string{"read", "propose"},
			Databases: []string{"*"}},
		"billing": {Name: "Billing bot", Scopes: []string{"read", "propose"},
			Databases: []string{"billing"}},
	} {
		req.Kind, req.ExpiresIn, req.CreatedBy = mcptoken.KindAgent, 24*time.Hour, "admin"
		tok, err := tokens.Create(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[label] = tok.Secret
		if label == "billing" {
			f.tokens["billing-id"] = tok.ID
		}
	}
	return f
}

func TestLive_OpenInvestigateAndReadTheCitedResult(t *testing.T) {
	f := newLiveFixture(t)
	ctx := context.Background()
	w := call(t, f.handler, "POST", base+"/databases/orders/investigations", f.tokens["read"],
		`{"symptom":{"summary":"checkout timeouts"},"family":"lock_blocking",
		"external_ref":{"system":"datadog","id":"monitor-1"}}`)
	var open OpenResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &open) != nil {
		t.Fatalf("open %d %s", w.Code, w.Body.String())
	}
	if err := f.coord.Investigate(ctx, sre.UUID(open.Investigation.ID)); err != nil {
		t.Fatal(err)
	}
	w = call(t, f.handler, "GET", open.Links.Result, f.tokens["read"], "")
	var r Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &r) != nil {
		t.Fatalf("result %d %s", w.Code, w.Body.String())
	}
	if r.Outcome != "concluded" || r.RootCause == nil || r.RootCause.Node !=
		"idle_in_tx_holder" || r.RootCause.Source != "graph" || !r.ChainVerified ||
		len(r.CausalChain) == 0 || r.Confidence.Calibration != "uncalibrated" {
		t.Fatalf("result %s", w.Body.String())
	}
	cited := map[string]bool{}
	for _, e := range r.Evidence {
		cited[e.ID] = true
	}
	numbers := false
	for _, c := range r.CausalChain[0].Evidence {
		if !cited[c.EvidenceID] {
			t.Fatalf("citation %s is not in the evidence index", c.EvidenceID)
		}
		numbers = numbers || c.Numbers["blocker_pid"] == 4242
	}
	if !numbers {
		t.Fatalf("the root's citations carry the evidence numbers: %+v", r.CausalChain[0])
	}
	if r.CallerSupplied == nil || r.CallerSupplied.Summary != "checkout timeouts" ||
		r.CallerSupplied.ExternalRef.ID != "monitor-1" {
		t.Fatalf("caller supplied %+v", r.CallerSupplied)
	}
	assertAuditNamesTheAgent(t, f, open.Investigation.ID)
}

// assertAuditNamesTheAgent: the investigation's created event and the
// request row name the agent.
func assertAuditNamesTheAgent(t *testing.T, f *liveFixture, invID string) {
	t.Helper()
	ctx := context.Background()
	events, verified, err := f.svc.Events(ctx, sre.UUID(invID))
	if err != nil || !verified || len(events) == 0 || !strings.HasPrefix(events[0].Actor,
		"agent:datadog:") {
		t.Fatalf("event chain actor: %+v %t %v", events, verified, err)
	}
	var actor, name string
	if err := f.pool.QueryRow(ctx, `SELECT actor, identity_name FROM sage.specialist_requests
		WHERE investigation_id = $1 AND kind = 'open'`, invID).Scan(&actor,
		&name); err != nil || name != "Datadog" || actor != events[0].Actor {
		t.Fatalf("request row %q %q %v", actor, name, err)
	}
}

func TestLive_ScopesDatabasesAndRevocation(t *testing.T) {
	f := newLiveFixture(t)
	ctx := context.Background()
	w := call(t, f.handler, "POST", base+"/databases/orders/investigations",
		f.tokens["billing"], `{"symptom":{"summary":"x"}}`)
	if w.Code != 403 || errorBody(t, w).Code != "database_not_permitted" {
		t.Fatalf("billing token on orders: %d %s", w.Code, w.Body.String())
	}
	w = call(t, f.handler, "POST", base+"/databases/orders/investigations", f.tokens["propose"],
		`{"symptom":{"summary":"x"},"family":"lock_blocking"}`)
	var open OpenResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &open) != nil {
		t.Fatalf("open %d %s", w.Code, w.Body.String())
	}
	if err := f.coord.Investigate(ctx, sre.UUID(open.Investigation.ID)); err != nil {
		t.Fatal(err)
	}
	rem := base + "/databases/orders/investigations/" + open.Investigation.ID +
		"/remediations/custodian.0123456789abcdef/request"
	if w = call(t, f.handler, "POST", rem, f.tokens["read"], ``); w.Code != 403 ||
		errorBody(t, w).Code != "scope_required" {
		t.Fatalf("read token remediation: %d %s", w.Code, w.Body.String())
	}
	if w = call(t, f.handler, "POST", rem, f.tokens["propose"], ``); w.Code != 404 {
		t.Fatalf("unknown remediation: %d %s", w.Code, w.Body.String())
	}
	if _, err := mcptoken.NewStore(f.pool).Revoke(ctx, f.tokens["billing-id"],
		"admin"); err != nil {
		t.Fatal(err)
	}
	w = call(t, f.handler, "GET", base+"/databases/billing/investigations/"+
		open.Investigation.ID, f.tokens["billing"], "")
	if w.Code != 401 || errorBody(t, w).Code != "unauthenticated" {
		t.Fatalf("revoked token: %d %s", w.Code, w.Body.String())
	}
}
