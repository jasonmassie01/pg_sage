//go:build cgo

package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// fakeRoles reports whether a principal has roles on a cluster.
type fakeRoles struct {
	status string // "" = no roles
	err    error
}

func (f fakeRoles) ClusterRolesOf(_ context.Context, pid, key string) (*agentguard.ClusterRole,
	error) {
	if f.err != nil || f.status == "" {
		return nil, f.err
	}
	return &agentguard.ClusterRole{PrincipalID: pid, ClusterKey: key, Status: f.status,
		BrokerRole: agentguard.BrokerRoleName(pid)}, nil
}

func (f *fixture) whoamiBroker(t *testing.T, roles fakeRoles, trust string) *Broker {
	t.Helper()
	b, err := New(f.cfg, Deps{Targets: fakeTargets{"db": f.target}, Logins: f.logins,
		Decider: f.decider, Classes: StoreClasses{}, Audit: &memAudit{}, Roles: roles,
		Databases:  func(context.Context) []string { return []string{"db", "gone"} },
		TrustLevel: func() string { return trust }})
	require.NoError(t, err)
	t.Cleanup(b.Close)
	return b
}

func TestWhoAmIDescribesThePrincipal(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.target.Env, f.target.Verified = envbind.EnvProd, true
	exec(t, f.super, fmt.Sprintf("GRANT SELECT (id) ON %s.hidden TO %s", ident(f.schema),
		ident(f.role)))
	b := f.whoamiBroker(t, fakeRoles{status: agentguard.RoleStatusActive}, "advisory")
	w, err := b.WhoAmI(f.ctx())
	require.NoError(t, err)
	if w.Principal.ID != f.p.ID || w.Principal.Name != f.p.Name ||
		w.Principal.Profile != "readonly-analyst" || w.Principal.EnvCeiling != "prod" ||
		w.Principal.Status != "active" {
		t.Errorf("principal = %+v", w.Principal)
	}
	if w.Sponsor == nil || w.Sponsor.UserID != 1 || !w.Sponsor.Active {
		t.Errorf("sponsor = %+v", w.Sponsor)
	}
	if w.Frozen || w.Tainted || w.Notice != "" {
		t.Errorf("frozen %v tainted %v notice %q", w.Frozen, w.Tainted, w.Notice)
	}
	if len(w.Databases) != 1 {
		t.Fatalf("databases = %+v, want db only (gone does not resolve)", w.Databases)
	}
	db := w.Databases[0]
	if db.Name != "db" || db.Env != "prod" || !db.BindingVerified ||
		len(db.Lanes) != 1 || db.Lanes[0] != "brokered" || db.Levels["read"] != 3 {
		t.Errorf("database = %+v", db)
	}
	var table, column bool
	for _, g := range db.Grants {
		switch g.Object {
		case f.schema + ".items":
			table = g.Capability == "read" && len(g.Columns) == 0
		case f.schema + ".hidden":
			column = g.Capability == "read" && len(g.Columns) == 1 && g.Columns[0] == "id"
		}
	}
	if !table || !column {
		t.Errorf("grants = %+v, want items (table) and hidden.id (column)", db.Grants)
	}
}

func TestWhoAmIStates(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.p.Status, f.p.Tainted = agentguard.StatusFrozen, true
	f.decider.verdict = decide.Verdict{Reason: agentguard.ReasonFrozen, Step: "D1",
		Principal: f.p}
	b := f.whoamiBroker(t, fakeRoles{}, "observation")
	w, err := b.WhoAmI(withAgent(context.Background(), f.p, []string{"db"}))
	require.NoError(t, err)
	if !w.Frozen || !w.Tainted {
		t.Errorf("frozen %v tainted %v, want both", w.Frozen, w.Tainted)
	}
	if !strings.Contains(w.Notice, "observation") {
		t.Errorf("notice %q, want the observation trust level named", w.Notice)
	}
	if len(w.Databases) != 1 || w.Databases[0].Levels["read"] != 0 ||
		len(w.Databases[0].Lanes) != 0 || w.Databases[0].Reason != "agent_frozen" {
		t.Errorf("databases = %+v, want read level 0, no lane, reason agent_frozen",
			w.Databases)
	}
	if _, err := b.WhoAmI(context.Background()); !errors.Is(err, agentguard.ErrNoPrincipal) {
		t.Errorf("no principal: error = %v, want ErrNoPrincipal", err)
	}
	broken := f.whoamiBroker(t, fakeRoles{err: errors.New("control down")}, "advisory")
	if _, err := broken.WhoAmI(f.ctx()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("role registry down: error = %v, want ErrUnavailable", err)
	}
}
