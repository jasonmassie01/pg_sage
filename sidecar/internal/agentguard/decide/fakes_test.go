package decide

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Test doubles for the decider's sources. Each records its calls so tests
// can assert that a step ran (or did not).

const testID = "agp_aaaaaaaaaaaaaaaaaaaa"

var errBoom = errors.New("control database connection refused")

func sponsor() *int { id := 7; return &id }

func activePrincipal() agentguard.Principal {
	return agentguard.Principal{ID: testID, Name: "coder", SponsorUserID: sponsor(),
		SponsorActive: true, Profile: "coding-agent", EnvCeiling: agentguard.EnvProd,
		Status: agentguard.StatusActive}
}

type fakePrincipals struct {
	mu    sync.Mutex
	p     agentguard.Principal
	err   error
	calls int
}

func (f *fakePrincipals) Get(_ context.Context, id string) (agentguard.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return agentguard.Principal{}, f.err
	}
	if id != f.p.ID {
		return agentguard.Principal{}, agentguard.ErrNotFound
	}
	return f.p, nil
}

type fakeEnvs struct {
	env envbind.Env
	err error
}

func (f fakeEnvs) Environment(_ context.Context, db string) (envbind.Binding, error) {
	if f.err != nil {
		return envbind.Binding{}, f.err
	}
	return envbind.Binding{Env: f.env, Evidence: envbind.Evidence{Database: db,
		Effective: f.env, Verified: f.env != envbind.EnvProd}}, nil
}

type fakeProfiles map[string]Profile

func (f fakeProfiles) Profile(name string) (Profile, bool) {
	p, ok := f[name]
	return p, ok
}

func codingProfiles() fakeProfiles {
	return fakeProfiles{"coding-agent": {Classes: []Capability{CapRead, CapWriteInsert,
		CapWriteUpdate, CapWriteDelete, CapDDLAdditive, CapDDLLocking, CapSandbox},
		EnvCeiling: envbind.EnvStage}}
}

type fakeFreezes struct {
	frozen bool
	reason string
	err    error
}

func (f fakeFreezes) Frozen(context.Context, string, string) (bool, string, error) {
	return f.frozen, f.reason, f.err
}

type fakeObjects struct{ err error }

func (f fakeObjects) CheckObjects(context.Context, agentguard.Principal, string,
	envbind.Env, []Object) error {
	return f.err
}

type fakeRecovery struct {
	pitr  bool
	drill time.Time
	err   error
}

func (f fakeRecovery) Recovery(context.Context, string) (bool, time.Time, error) {
	return f.pitr, f.drill, f.err
}

type fakeLeases struct {
	active bool
	err    error
}

func (f fakeLeases) LeaseActive(context.Context, string, string, string) (bool, error) {
	return f.active, f.err
}

var testNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// fullConfig is every source present and permissive, on a stage database.
func fullConfig(p agentguard.Principal) (Config, *fakePrincipals) {
	principals := &fakePrincipals{p: p}
	return Config{
		Principals:   principals,
		Environments: fakeEnvs{env: envbind.EnvStage},
		Profiles:     codingProfiles(),
		Freezes:      fakeFreezes{},
		Objects:      fakeObjects{},
		ChangeFreeze: func(context.Context, string) (bool, error) { return false, nil },
		Recovery:     fakeRecovery{pitr: true, drill: testNow.Add(-24 * time.Hour)},
		Pending:      func(context.Context, string) (int, error) { return 0, nil },
		Leases:       fakeLeases{active: true},
		Now:          func() time.Time { return testNow },
	}, principals
}

func agentRead() Request {
	return Request{PrincipalID: testID, Tool: "agent_query", Kind: agentguard.ToolAgent,
		Capability: CapRead, Database: "orders",
		Objects: []Object{{Schema: "app", Relation: "orders", Column: "id"}},
		GrantID: "grant-1"}
}

func proposeMigration() Request {
	return Request{PrincipalID: testID, Tool: "apply_migration",
		Kind: agentguard.ToolPropose, Capability: CapDDLAdditive, Database: "orders"}
}
