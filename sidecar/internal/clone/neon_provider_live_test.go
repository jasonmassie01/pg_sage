package clone_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clone/contracttest"
)

// lab-05b CHECK-31 (OWNER ACTION, decision D2): the clone contract on a
// real, funded Neon project. It creates billable branches with production
// data, so it runs only with SAGE_NEON_LIVE=1 and
//
//	SAGE_NEON_API_KEY            a project-scoped Neon API key
//	SAGE_NEON_PROJECT_ID         the project the monitored database is in
//	SAGE_NEON_LIVE_DATABASE_URL  the monitored (production) database
//	SAGE_TEST_DATABASE_URL       a scratch database for the receipts
//
// Afterwards `neon branches list` must show no sage-* branch.
const neonLiveFlag = "SAGE_NEON_LIVE"

func liveNeonEnv(t *testing.T) (key, project, dsn string) {
	t.Helper()
	contracttest.RequireLive(t, neonLiveFlag)
	key, project = os.Getenv("SAGE_NEON_API_KEY"), os.Getenv("SAGE_NEON_PROJECT_ID")
	dsn = os.Getenv("SAGE_NEON_LIVE_DATABASE_URL")
	if strings.TrimSpace(key) == "" || project == "" || dsn == "" {
		t.Fatal(neonLiveFlag + " is set but SAGE_NEON_API_KEY, SAGE_NEON_PROJECT_ID or " +
			"SAGE_NEON_LIVE_DATABASE_URL is not")
	}
	return key, project, dsn
}

func TestNeonLive_ContractOnARealProject(t *testing.T) {
	key, project, dsn := liveNeonEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prodPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid SAGE_NEON_LIVE_DATABASE_URL")
	}
	t.Cleanup(prodPool.Close)
	identity, err := clone.PoolIdentity(prodPool)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn := prodPool.Config().ConnConfig
	prod := clone.NeonProduction{Host: conn.Host, User: conn.User, Database: conn.Database}
	pool := neonReceiptPool(t)
	contracttest.Run(t, func(t *testing.T) contracttest.Subject {
		p, err := clone.NewNeonBranchProvider(clone.NeonConfig{ProjectID: project,
			APIKey: key, EndpointMaxCU: 0.25, TTL: time.Hour,
			Production: func(context.Context) (clone.NeonProduction, error) {
				return prod, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Subject{Adapter: clone.NeonAdapter, Provider: p,
			Production: identity, Exists: liveNeonExists(key, project)}
	}, contracttest.Options{Pool: pool, DeploymentID: neonDeployment})
}

// liveNeonExists reports a branch as live while the Neon API still
// describes it, asking Neon directly rather than through the provider.
func liveNeonExists(key, project string) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, ref string) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://console.neon.tech/api/v2/projects/"+url.PathEscape(project)+
				"/branches/"+url.PathEscape(ref), nil)
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			return true, nil
		case http.StatusNotFound:
			return false, nil
		default:
			return false, fmt.Errorf("get branch %s: status %d", ref, resp.StatusCode)
		}
	}
}
