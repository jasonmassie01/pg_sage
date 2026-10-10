package decide

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
)

// D1, D2 and D8 read the principal on every decision, not the snapshot
// taken when the token authenticated (or the stdio session started): a
// freeze, retirement or taint takes effect on the very next request of a
// live session.

func liveDecider(pool *agentguard.Store) *Decider {
	return New(Config{Principals: pool, Profiles: DefaultProfiles(),
		Environments: fakeEnvs{env: envbind.EnvDev}})
}

func TestLiveSessionSeesFreezeAndRetireImmediately(t *testing.T) {
	pool := livePool(t)
	store := agentguard.NewStore(pool)
	id := livePrincipal(t, pool, agentguard.StatusActive)
	d := liveDecider(store)
	// The identity a token or stdio session carries is bound once.
	ctx := policy.WithPrincipalRef(context.Background(),
		policy.PrincipalRef{ID: id, Tool: "apply_migration"})
	ref, _ := policy.PrincipalRefFromContext(ctx)
	req := policy.ActionRequest{Principal: &ref, SQL: "CREATE TABLE app.t (id int)"}
	gate := d.Policy("orders")
	if _, level, stop := gate.Decide(ctx, req); stop || level != 2 {
		t.Fatalf("active = (%d, %v), want allowed at L2", level, stop)
	}
	if _, err := store.SetStatus(context.Background(), id, agentguard.StatusFrozen,
		"kill"); err != nil {
		t.Fatal(err)
	}
	dec, _, stop := gate.Decide(ctx, req)
	if !stop || dec.Reason != policy.Reason(agentguard.ReasonFrozen) {
		t.Fatalf("after freeze = %+v, want agent_frozen on the same session", dec)
	}
	if _, err := store.SetStatus(context.Background(), id, agentguard.StatusRetired,
		""); err != nil {
		t.Fatal(err)
	}
	dec, _, stop = gate.Decide(ctx, req)
	if !stop || dec.Reason != policy.Reason(agentguard.ReasonRetired) {
		t.Fatalf("after retire = %+v, want agent_retired on the same session", dec)
	}
}

func TestLiveSessionSeesTaintImmediately(t *testing.T) {
	pool := livePool(t)
	store := agentguard.NewStore(pool)
	sponsorID, err := auth.CreateUser(context.Background(), pool,
		fmt.Sprintf("sponsor-%d@example.com", time.Now().UnixNano()), "password-123",
		"operator")
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.Create(context.Background(), agentguard.CreateRequest{
		Name:          fmt.Sprintf("taint-%d-%d", time.Now().UnixNano()%1e9, seq.Add(1)),
		SponsorUserID: &sponsorID, Profile: "readonly-analyst",
		EnvCeiling: agentguard.EnvProd, CreatedBy: "admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	id := p.ID
	d := liveDecider(store)
	req := Request{PrincipalID: id, Tool: "agent_query", Kind: agentguard.ToolAgent,
		Capability: CapRead, Database: "orders"}
	if v := d.Decide(context.Background(), req); !v.Allowed || v.MaxLevel != 3 {
		t.Fatalf("clean = %+v, want L3", v)
	}
	if err := store.Taint(context.Background(), id, "", "read app.inbox"); err != nil {
		t.Fatal(err)
	}
	if v := d.Decide(context.Background(), req); !v.Allowed || v.MaxLevel != 2 {
		t.Fatalf("tainted = %+v, want capped at L2 on the next request", v)
	}
}
