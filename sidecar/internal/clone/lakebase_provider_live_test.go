package clone

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/lakebaseid"
)

// TestLakebaseLive_BranchRehearsalRoundTrip creates a real branch of the
// configured endpoint's branch, rehearses an expand-only migration on it,
// deletes it, and checks the branch is gone (market-03 CHECK-30). It
// creates billable resources, so it runs only when asked.
func TestLakebaseLive_BranchRehearsalRoundTrip(t *testing.T) {
	if os.Getenv("SAGE_LAKEBASE_LIVE") != "1" {
		t.Skip("live Lakebase branch rehearsal: set SAGE_LAKEBASE_LIVE=1, " +
			"SAGE_LAKEBASE_ENDPOINT, SAGE_LAKEBASE_LIVE_DATABASE_URL (passwordless) and " +
			"Databricks unified auth with CAN_MANAGE on the project")
	}
	endpoint := os.Getenv("SAGE_LAKEBASE_ENDPOINT")
	dsn := os.Getenv("SAGE_LAKEBASE_LIVE_DATABASE_URL")
	if _, _, _, err := lakebaseid.Parse(endpoint); err != nil || dsn == "" {
		t.Fatal("live branch rehearsal needs SAGE_LAKEBASE_ENDPOINT and " +
			"SAGE_LAKEBASE_LIVE_DATABASE_URL")
	}
	conn, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid SAGE_LAKEBASE_LIVE_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pg, err := databricksauth.Postgres()
	if err != nil {
		t.Fatal(err)
	}
	p := liveLakebaseProvider(t, pg, endpoint, conn)
	c, err := p.Create(ctx, CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatal(err)
	}
	destroyed := false
	defer func() {
		if !destroyed {
			_ = p.Destroy(context.Background(), c)
		}
	}()
	rehearseOnLiveBranch(ctx, t, c.DSN)
	if err := p.Destroy(ctx, c); err != nil {
		t.Fatal(err)
	}
	destroyed = true
	_, err = pg.GetBranch(ctx, postgres.GetBranchRequest{Name: c.ID})
	if !errors.Is(err, apierr.ErrNotFound) {
		t.Fatalf("branch %s after destroy: %v, want not found", c.ID, err)
	}
}

// liveLakebaseProvider branches from the endpoint's branch, with
// production's identity read from live metadata as the sidecar does.
func liveLakebaseProvider(t *testing.T, pg postgres.PostgresInterface, endpoint string,
	conn *pgx.ConnConfig) *LakebaseBranchProvider {
	t.Helper()
	project, branch, _, err := lakebaseid.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewLakebaseBranchProvider(LakebaseConfig{Project: project, TTL: time.Hour,
		EndpointMaxCU: 1, API: func() (LakebaseBranchAPI, error) {
			return NewLakebaseSDKAPI(pg), nil
		},
		Production: func(ctx context.Context) (LakebaseProduction, error) {
			res, err := databricksauth.ResolveTelemetryResource(ctx, conn.Host, endpoint)
			return LakebaseProduction{BranchName: "projects/" + project + "/branches/" +
				branch, BranchUID: res.BranchID, Host: conn.Host, User: conn.User,
				Database: conn.Database}, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// rehearseOnLiveBranch runs an expand-only migration on the branch.
func rehearseOnLiveBranch(ctx context.Context, t *testing.T, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the branch: %v", strings.ReplaceAll(err.Error(), dsn, "<dsn>"))
	}
	defer conn.Close(context.Background())
	for _, stmt := range []string{
		"CREATE TABLE sage_rehearsal_probe (id bigint PRIMARY KEY)",
		"ALTER TABLE sage_rehearsal_probe ADD COLUMN note text",
		"CREATE INDEX sage_rehearsal_probe_note ON sage_rehearsal_probe (note)",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("rehearse %q: %v", stmt, err)
		}
	}
}
