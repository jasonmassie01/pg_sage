package principalfile

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcpauth"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/principalfile"))
}

func coreDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.users (email, password, role)
			VALUES ($1, 'x', 'admin')`, email); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	return pool
}

func coreApply(t *testing.T, pool *pgxpool.Pool, f *File) *Plan {
	t.Helper()
	ctx := context.Background()
	b, cat := NewCoreBackend(pool), NewCoreCatalog(pool, SpecProfiles())
	p, err := MakePlan(ctx, f, b, cat)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := Apply(ctx, p, f, b, ApplyOptions{Actor: "alice",
		ApproveWidening: true}); err != nil {
		t.Fatalf("apply %s: %v", p.Render(), err)
	}
	again, err := MakePlan(ctx, f, b, cat)
	if err != nil || len(again.Changes) != 0 {
		t.Fatalf("not converged (%v): %s", err, again.Render())
	}
	return p
}

// The file drives core's principal store end to end: principals are
// created with their sponsor, frozen status and ownership, identities are
// bound where the MCP OAuth resolver reads them, and a second plan is
// empty.
func TestCoreBackendAppliesAFile(t *testing.T) {
	pool := coreDB(t, "pf_core_apply")
	ctx := context.Background()
	coreApply(t, pool, mustParse(t, validFile))
	store := agentguard.NewStore(pool)
	ci, err := store.GetByName(ctx, "ci-bot")
	if err != nil || ci.EnvCeiling != "stage" || ci.Profile != "readonly-analyst" ||
		ci.SponsorUserID == nil || ci.CreatedBy != "alice" {
		t.Fatalf("ci-bot = %+v, %v", ci, err)
	}
	etl, err := store.GetByName(ctx, "etl-writer")
	if err != nil || !etl.Frozen() {
		t.Fatalf("etl-writer = %+v, %v (want frozen)", etl, err)
	}
	id, err := mcpauth.NewDBResolver(pool).PrincipalForSubject(ctx,
		"https://idp.example.com", "0oa1")
	if err != nil || id != ci.ID {
		t.Fatalf("OAuth binding resolves to %q, %v; want %s", id, err, ci.ID)
	}
}

// Narrowing edits and removal reach core: a lower ceiling, an unbound
// identity and, under prune, a retired principal.
func TestCoreBackendNarrowsAndRetires(t *testing.T) {
	pool := coreDB(t, "pf_core_narrow")
	ctx := context.Background()
	coreApply(t, pool, mustParse(t, validFile))
	f := mustParse(t, validFile)
	f.Spec.Principals[0].EnvCeiling = "dev"
	f.Spec.Principals[0].Identities = nil
	f.Spec.Principals = f.Spec.Principals[:1]
	p := coreApply(t, pool, f)
	if p.Widening() {
		t.Fatalf("a narrowing edit was marked widening: %s", p.Render())
	}
	store := agentguard.NewStore(pool)
	if ci, _ := store.GetByName(ctx, "ci-bot"); ci.EnvCeiling != "dev" {
		t.Fatalf("ci-bot ceiling = %s", ci.EnvCeiling)
	}
	if etl, _ := store.GetByName(ctx, "etl-writer"); !etl.Retired() {
		t.Fatalf("etl-writer not retired: %+v", etl)
	}
	_, err := mcpauth.NewDBResolver(pool).PrincipalForSubject(ctx,
		"https://idp.example.com", "0oa1")
	if !errors.Is(err, mcpauth.ErrNoBinding) {
		t.Fatalf("unbound identity still resolves: %v", err)
	}
}

// A principal created outside the file (the API) is adopted, then managed.
func TestCoreBackendAdoptsAndChecksCatalog(t *testing.T) {
	pool := coreDB(t, "pf_core_adopt")
	ctx := context.Background()
	if _, err := agentguard.NewStore(pool).Create(ctx, agentguard.CreateRequest{
		Name: "ci-bot", Profile: "readonly-analyst", EnvCeiling: "stage",
		CreatedBy: "user:1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	p := coreApply(t, pool, mustParse(t, validFile))
	if !strings.Contains(ops(p), "adopt ci-bot managed_by") {
		t.Fatalf("adoption plan = %s", ops(p))
	}
	cat := NewCoreCatalog(pool, SpecProfiles())
	if !cat.UserExists("alice@example.com") || cat.UserExists("mallory@example.com") {
		t.Fatalf("user lookup wrong")
	}
	if _, ok := cat.ProfileClasses("god-mode"); ok {
		t.Fatalf("unknown profile accepted")
	}
}

// Removing a sponsor is refused (core cannot unset one through Update);
// a storage failure surfaces as an error.
func TestCoreBackendErrors(t *testing.T) {
	pool := coreDB(t, "pf_core_errors")
	ctx := context.Background()
	coreApply(t, pool, mustParse(t, validFile))
	f := mustParse(t, validFile)
	f.Spec.Principals[0].Sponsor = ""
	b := NewCoreBackend(pool)
	p, err := MakePlan(ctx, f, b, NewCoreCatalog(pool, SpecProfiles()))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := Apply(ctx, p, f, b, ApplyOptions{Actor: "alice"}); err == nil ||
		!strings.Contains(err.Error(), "sponsor") {
		t.Fatalf("sponsor removal: err = %v", err)
	}
	pool.Close()
	if _, err := b.List(ctx); err == nil {
		t.Fatalf("closed pool: no error")
	}
}
