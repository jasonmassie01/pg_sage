package databricksauth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdktime "github.com/databricks/databricks-sdk-go/common/types/time"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T, ssl string) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://test@db.example/test?sslmode=" + ssl)
	if err != nil {
		t.Fatal(err)
	}
	// Model a preexisting bootstrap credential without embedding one in a URL.
	cfg.ConnConfig.Password = "bootstrap"
	return cfg
}

func TestLakebasePoolRefreshesWithoutRestartAndCachesConcurrentConnections(t *testing.T) {
	cfg := testPool(t, "require")
	var calls int
	expires := time.Now().Add(time.Hour)
	first := &postgres.DatabaseCredential{Token: "first-test-token", ExpireTime: sdktime.New(expires)}
	if err := configurePool(cfg, []string{"db.example"}, func(context.Context) (*postgres.DatabaseCredential, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		freshExpiry := time.Now().Add(time.Hour)
		return &postgres.DatabaseCredential{Token: "refreshed-test-token", ExpireTime: sdktime.New(freshExpiry)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Password != "" {
		t.Fatal("pool config retained bootstrap password")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			conn := cfg.ConnConfig.Copy()
			if err := cfg.BeforeConnect(context.Background(), conn); err != nil {
				t.Error(err)
			} else if conn.Password != "first-test-token" {
				t.Error("connection received wrong credential")
			}
		})
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("mint calls=%d; want one for concurrent pool growth", calls)
	}
	soon := time.Now().Add(time.Minute)
	first.ExpireTime = sdktime.New(soon)
	conn := cfg.ConnConfig.Copy()
	if err := cfg.BeforeConnect(context.Background(), conn); err != nil || conn.Password != "refreshed-test-token" || calls != 2 {
		t.Fatalf("refresh failed: %v (mint calls=%d)", err, calls)
	}
	if cfg.ConnConfig.Password != "" {
		t.Fatal("pool config exposed a generated credential")
	}
}

func TestLakebasePoolRefusesWrongHostAndPlaintextFallbacks(t *testing.T) {
	for _, ssl := range []string{"disable", "prefer", "allow"} {
		if err := configurePool(testPool(t, ssl), []string{"db.example"}, nil); err == nil {
			t.Errorf("accepted sslmode=%s", ssl)
		}
	}
	if err := configurePool(testPool(t, "require"), []string{"different.example"}, nil); err == nil {
		t.Fatal("accepted host not belonging to the endpoint")
	}
}

func TestLakebaseRefreshFailureStopsConnection(t *testing.T) {
	cfg := testPool(t, "require")
	failed := errors.New("credential service unavailable")
	if err := configurePool(cfg, []string{"db.example"}, func(context.Context) (*postgres.DatabaseCredential, error) {
		return nil, failed
	}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.BeforeConnect(context.Background(), cfg.ConnConfig.Copy()); !errors.Is(err, failed) {
		t.Fatalf("refresh failure = %v", err)
	}
}

func TestMalformedLakebaseMappingFailsBeforeAuthentication(t *testing.T) {
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"db.example":"projects/bad"}`)
	if err := ConfigurePool(context.Background(), testPool(t, "require")); err == nil {
		t.Fatal("accepted invalid endpoint mapping")
	}
}

func TestLakebaseOAuthRejectsPooledEndpointHosts(t *testing.T) {
	hosts := &postgres.EndpointHosts{Host: "db.example", ReadOnlyHost: "readonly.example",
		ReadWritePooledHost: "pooled.example", ReadOnlyPooledHost: "readonly-pooled.example"}
	for _, host := range []string{"pooled.example", "readonly-pooled.example"} {
		cfg := testPool(t, "require")
		cfg.ConnConfig.Host = host
		if err := configureEndpointPool(cfg, hosts, nil); err == nil {
			t.Errorf("OAuth accepted pooled host %s", host)
		}
	}
	for _, host := range []string{"db.example", "readonly.example"} {
		cfg := testPool(t, "require")
		cfg.ConnConfig.Host = host
		if err := configureEndpointPool(cfg, hosts, nil); err != nil {
			t.Errorf("OAuth rejected direct host %s: %v", host, err)
		}
	}
}

func TestLakebaseSingleConnectionRetainsOrdinaryAuthentication(t *testing.T) {
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	cfg := testPool(t, "require").ConnConfig
	if err := ConfigureConnection(t.Context(), cfg); err != nil || cfg.Password != "bootstrap" {
		t.Fatalf("ordinary connection auth changed: %v", err)
	}
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"db.example":"projects/invalid"}`)
	if err := ConfigureConnection(t.Context(), cfg); err == nil {
		t.Fatal("single connection skipped endpoint validation")
	}
}
