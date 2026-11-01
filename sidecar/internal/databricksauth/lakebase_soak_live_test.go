package databricksauth

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLakebaseLiveCredentialExpiry waits for a real credential to expire. It
// exercises the production credential cache and refresh hook, without changing
// database objects. Constant-query held-session liveness is checked separately
// from fresh authentication; this does not establish table or UC-claim lifetime.
func TestLakebaseLiveCredentialExpiry(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE_SOAK") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE_SOAK=1 for the real credential-expiry soak")
	}
	endpoint := os.Getenv("SAGE_LAKEBASE_ENDPOINT")
	if !strings.Contains(endpoint, "/branches/pg-sage-test/endpoints/") {
		t.Fatal("expiry soak requires the explicitly isolated pg-sage-test branch")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_LAKEBASE_LIVE_DATABASE_URL"))
	if err != nil || cfg == nil || cfg.ConnConfig.Database != "pg_sage_test" || cfg.ConnConfig.Password != "" {
		t.Fatal("expiry soak requires a passwordless pg_sage_test DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Minute)
	defer cancel()
	w, err := workspace()
	if err != nil {
		t.Fatal(err)
	}
	ep, err := w.Postgres.GetEndpoint(ctx, postgres.GetEndpointRequest{Name: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Status == nil || ep.Status.Hosts == nil {
		t.Fatal("endpoint hosts unavailable")
	}
	var minted int
	var firstExpiry, lastExpiry time.Time
	err = configureEndpointPool(cfg, ep.Status.Hosts, func(ctx context.Context) (*postgres.DatabaseCredential, error) {
		credential, err := w.Postgres.GenerateDatabaseCredential(ctx, postgres.GenerateDatabaseCredentialRequest{Endpoint: endpoint})
		if err == nil && credential != nil && credential.ExpireTime != nil {
			minted++
			lastExpiry = credential.ExpireTime.AsTime()
			if firstExpiry.IsZero() {
				firstExpiry = lastExpiry
			}
		}
		return credential, err
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.MinConns, cfg.MaxConns = 0, 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if held != nil {
			held.Release()
		}
	}()
	var firstPID int
	if err := held.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&firstPID); err != nil {
		t.Fatal(err)
	}
	wake := firstExpiry.Add(30 * time.Second)
	if time.Until(wake) > 80*time.Minute || time.Until(wake) < time.Minute {
		t.Fatalf("unexpected credential lifetime: expires %s", firstExpiry.UTC())
	}
	t.Logf("credential minted; expiry=%s; awaiting real expiry until %s", firstExpiry.UTC().Format(time.RFC3339), wake.UTC().Format(time.RFC3339))
	heldHealthy := true
	for time.Now().Before(wake) {
		remaining := time.Until(wake)
		if remaining > time.Minute {
			remaining = time.Minute
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
		if held != nil {
			probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Second)
			var value int
			err := held.QueryRow(probeCtx, "SELECT 1").Scan(&value)
			probeCancel()
			if err != nil {
				heldHealthy = false
				t.Logf("held-session continuity lost before/at expiry (%T); continuing original cached-credential expiry check", err)
				held.Release()
				held = nil
			}
		}
		t.Logf("held session healthy=%v; original credential expiry in %s", heldHealthy, time.Until(firstExpiry).Round(time.Second))
	}
	pool.Reset()
	if held != nil {
		held.Release()
		held = nil
	}
	var secondPID int
	if err := pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&secondPID); err != nil {
		t.Fatal(err)
	}
	if secondPID == firstPID || minted < 2 || !lastExpiry.After(firstExpiry) {
		t.Fatalf("new-session refresh unproven: PID changed=%v, credentials=%d, expiry advanced=%v", secondPID != firstPID, minted, lastExpiry.After(firstExpiry))
	}
	t.Logf("PASS: new backend authenticated with refreshed credential after original expiry (mint count=%d)", minted)
	if !heldHealthy {
		t.Error("held native-session continuity was interrupted; fresh-session credential refresh passed but uninterrupted held-session compatibility is unverified")
	}
}
