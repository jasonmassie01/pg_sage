package api

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// monitorOnly creates a login role with pg_monitor and CONNECT only (no
// SELECT on any sequence) and returns a pool connected as it.
func monitorOnly(t *testing.T, ctx context.Context, admin *pgxpool.Pool,
	dsn string) *pgxpool.Pool {
	t.Helper()
	role := fmt.Sprintf("api_fl_mon_%06x", time.Now().UnixNano()&0xffffff)
	rq := pgx.Identifier{role}.Sanitize()
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("current database: %v", err)
	}
	for _, stmt := range []string{
		"CREATE ROLE " + rq + " LOGIN PASSWORD 'pw_" + role + "'",
		"GRANT pg_monitor TO " + rq,
		"GRANT CONNECT ON DATABASE " + pgx.Identifier{db}.Sanitize() + " TO " + rq,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP OWNED BY "+rq)
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+rq)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(role, "pw_"+role)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as monitor: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestOnboardingDegradedCountMatchesTheStoredReport: with sequence SELECT
// missing, the stored first look has a degraded sequence_runway with the
// GRANT, and /api/v1/onboarding reports exactly that many degraded checks.
func TestOnboardingDegradedCountMatchesTheStoredReport(t *testing.T) {
	stored := storedFirstLookWithoutSequenceAccess(t)
	assertStoredSequenceDegraded(t, stored)
	f := newFakeOnboarding()
	db := f.dbs["app"]
	db.FirstLook = &stored
	f.dbs["app"] = db
	_, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding?database=app")
	entry := body["databases"].([]any)[0].(map[string]any)
	fl := entry["first_look"].(map[string]any)
	want := firstlook.CountChecks(stored.Checks, "")
	if fl["degraded"] != float64(want.Degraded) || fl["degraded"] != float64(1) ||
		fl["checks"] != float64(want.Total) {
		t.Fatalf("first_look = %v, want degraded 1 of %d (stored %+v)", fl, want.Total,
			stored.Checks)
	}
	s := onboardingStep(t, entry, onboarding.StepFirstLook)
	if !strings.Contains(s["detail"].(string), fmt.Sprintf("1 of %d checks could not run",
		want.Total)) {
		t.Fatalf("first look step = %v", s)
	}
}

// storedFirstLookWithoutSequenceAccess takes a first look of a fresh
// database as a role that cannot read its sequence, stores it, and returns
// the report as the store reads it back.
func storedFirstLookWithoutSequenceAccess(t *testing.T) firstlook.Report {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "api_fl_degraded")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(admin.Close)
	if err := schema.Bootstrap(ctx, admin); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, stmt := range []string{"CREATE SCHEMA app",
		"CREATE TABLE app.orders (id serial PRIMARY KEY, note text)"} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	r, err := firstlook.Run(ctx, monitorOnly(t, ctx, admin, dsn),
		firstlook.Options{Database: "app", Provider: "self-managed"})
	if err != nil {
		t.Fatalf("first look: %v", err)
	}
	store := firstlook.NewStore(admin)
	if _, err := store.Save(ctx, r); err != nil {
		t.Fatalf("save: %v", err)
	}
	stored, ok, err := store.Latest(ctx, "app")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	return stored
}

func assertStoredSequenceDegraded(t *testing.T, r firstlook.Report) {
	t.Helper()
	for _, c := range r.Checks {
		if c.Rule != firstlook.RuleSequenceRunway {
			continue
		}
		if c.Status != firstlook.CheckDegraded ||
			!strings.Contains(c.Note, "GRANT SELECT ON ALL SEQUENCES IN SCHEMA app") {
			t.Fatalf("stored sequence check = %+v, want degraded with the GRANT", c)
		}
		return
	}
	t.Fatalf("no sequence_runway check in %+v", r.Checks)
}
