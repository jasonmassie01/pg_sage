package databricksauth

import (
	"context"
	"errors"
	"strings"
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
	cfg := testPool(t, "verify-full")
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
	err := configurePool(testPool(t, "verify-full"), []string{"different.example"}, nil)
	if err == nil {
		t.Fatal("accepted host not belonging to the endpoint")
	}
}

func TestLakebaseRefreshFailureStopsConnection(t *testing.T) {
	cfg := testPool(t, "verify-full")
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
	if err := ConfigurePool(context.Background(), testPool(t, "verify-full")); err == nil {
		t.Fatal("accepted invalid endpoint mapping")
	}
}

func TestLakebaseOAuthRejectsPooledEndpointHosts(t *testing.T) {
	hosts := &postgres.EndpointHosts{Host: "db.example", ReadOnlyHost: "readonly.example",
		ReadWritePooledHost: "pooled.example", ReadOnlyPooledHost: "readonly-pooled.example"}
	for _, host := range []string{"pooled.example", "readonly-pooled.example"} {
		cfg := testPool(t, "verify-full")
		// Retarget as pgx would parse this host: TLS stays bound to it.
		cfg.ConnConfig.Host, cfg.ConnConfig.TLSConfig.ServerName = host, host
		if err := configureEndpointPool(cfg, hosts, nil); err == nil {
			t.Errorf("OAuth accepted pooled host %s", host)
		}
	}
	for _, host := range []string{"db.example", "readonly.example"} {
		cfg := testPool(t, "verify-full")
		// Retarget as pgx would parse this host: TLS stays bound to it.
		cfg.ConnConfig.Host, cfg.ConnConfig.TLSConfig.ServerName = host, host
		if err := configureEndpointPool(cfg, hosts, nil); err != nil {
			t.Errorf("OAuth rejected direct host %s: %v", host, err)
		}
	}
}

func TestLakebaseSingleConnectionRetainsOrdinaryAuthentication(t *testing.T) {
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	cfg := testPool(t, "verify-full").ConnConfig
	if err := ConfigureConnection(t.Context(), cfg); err != nil || cfg.Password != "bootstrap" {
		t.Fatalf("ordinary connection auth changed: %v", err)
	}
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"db.example":"projects/invalid"}`)
	if err := ConfigureConnection(t.Context(), cfg); err == nil {
		t.Fatal("single connection skipped endpoint validation")
	}
}

// mintForbidden fails the test if a credential is minted: every case using it
// must be refused before a token exists.
func mintForbidden(t *testing.T) mintCredential {
	return func(context.Context) (*postgres.DatabaseCredential, error) {
		t.Error("minted a database credential for a refused connection")
		return nil, errors.New("mint must not be called")
	}
}

func requireUnverifiedTLSError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnverifiedTLS) {
		t.Fatalf("error = %v; want ErrUnverifiedTLS", err)
	}
	// Fleet databases take sslmode from YAML, not from the environment.
	wants := []string{"sslmode=verify-full", "SAGE_DATABASE_URL", "SAGE_PG_SSLMODE",
		"databases[].sslmode"}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestLakebaseOAuthRefusesUnverifiedTLS(t *testing.T) {
	// require and verify-ca encrypt but never bind the certificate to the
	// endpoint hostname, so a network attacker could capture the token.
	for _, ssl := range []string{"require", "verify-ca", "disable", "allow", "prefer"} {
		t.Run(ssl, func(t *testing.T) {
			cfg := testPool(t, ssl)
			err := configurePool(cfg, []string{"db.example"}, mintForbidden(t))
			requireUnverifiedTLSError(t, err)
			if cfg.BeforeConnect != nil {
				t.Fatal("refused pool still installed a credential hook")
			}
			if cfg.ConnConfig.Password != "bootstrap" {
				t.Fatal("refused pool changed the configured password")
			}
		})
	}
}

func TestLakebaseOAuthAcceptsVerifyFullOnEveryHost(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(
		"postgres://test@db.example,readonly.example/test?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ConnConfig.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %d; want 1", len(cfg.ConnConfig.Fallbacks))
	}
	mint := func(context.Context) (*postgres.DatabaseCredential, error) {
		return &postgres.DatabaseCredential{Token: "verified-test-token",
			ExpireTime: sdktime.New(time.Now().Add(time.Hour))}, nil
	}
	allowed := []string{"db.example", "readonly.example"}
	if err := configurePool(cfg, allowed, mint); err != nil {
		t.Fatalf("verify-full on every host refused: %v", err)
	}
	conn := cfg.ConnConfig.Copy()
	err = cfg.BeforeConnect(t.Context(), conn)
	if err != nil || conn.Password != "verified-test-token" {
		t.Fatalf("verified connection not authorized: %v", err)
	}
}

func TestLakebaseOAuthServerNameMustMatchHost(t *testing.T) {
	upper := testPool(t, "verify-full")
	upper.ConnConfig.TLSConfig.ServerName = "DB.EXAMPLE"
	if err := configurePool(upper, []string{"db.example"}, nil); err != nil {
		t.Fatalf("case-only ServerName difference refused: %v", err)
	}
	for _, name := range []string{"other.example", "", "db.example.attacker.example"} {
		t.Run("servername="+name, func(t *testing.T) {
			cfg := testPool(t, "verify-full")
			cfg.ConnConfig.TLSConfig.ServerName = name
			err := configurePool(cfg, []string{"db.example"}, mintForbidden(t))
			requireUnverifiedTLSError(t, err)
		})
	}
}

func multiHostPool(t *testing.T) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(
		"postgres://test@db.example,readonly.example/test?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestLakebaseOAuthRefusesUnverifiedFallback(t *testing.T) {
	allowed := []string{"db.example", "readonly.example"}
	insecure := multiHostPool(t)
	insecure.ConnConfig.Fallbacks[0].TLSConfig.InsecureSkipVerify = true
	requireUnverifiedTLSError(t, configurePool(insecure, allowed, mintForbidden(t)))

	renamed := multiHostPool(t)
	renamed.ConnConfig.Fallbacks[0].TLSConfig.ServerName = "db.example"
	requireUnverifiedTLSError(t, configurePool(renamed, allowed, mintForbidden(t)))

	plaintext := multiHostPool(t)
	plaintext.ConnConfig.Fallbacks[0].TLSConfig = nil
	requireUnverifiedTLSError(t, configurePool(plaintext, allowed, mintForbidden(t)))

	// The host allowlist is unchanged: a verified fallback to a host outside
	// the endpoint is still refused, and not as a TLS problem.
	foreign := multiHostPool(t)
	err := configurePool(foreign, []string{"db.example"}, mintForbidden(t))
	if err == nil || errors.Is(err, ErrUnverifiedTLS) {
		t.Fatalf("foreign fallback host = %v; want the host allowlist refusal", err)
	}
}

func TestLakebaseOAuthRefusesConnectionDowngradedAfterConfiguration(t *testing.T) {
	cfg := testPool(t, "verify-full")
	if err := configurePool(cfg, []string{"db.example"}, mintForbidden(t)); err != nil {
		t.Fatal(err)
	}
	conn := cfg.ConnConfig.Copy()
	conn.TLSConfig.InsecureSkipVerify = true
	requireUnverifiedTLSError(t, cfg.BeforeConnect(t.Context(), conn))
	if conn.Password != "" {
		t.Fatal("downgraded connection received a credential")
	}
}

// isolateDatabricksSDK points the SDK at an unroutable local workspace with a
// fake token, so a regression that reaches unified authentication fails fast
// and never uses the developer's real Databricks profile.
func isolateDatabricksSDK(t *testing.T) {
	t.Helper()
	t.Setenv("DATABRICKS_CONFIG_FILE", t.TempDir()+"/absent.cfg")
	t.Setenv("DATABRICKS_CONFIG_PROFILE", "")
	t.Setenv("DATABRICKS_HOST", "https://127.0.0.1:1")
	t.Setenv("DATABRICKS_AUTH_TYPE", "pat")
	t.Setenv("DATABRICKS_TOKEN", "test-not-a-real-token")
}

func TestLakebaseEndpointRefusesUnverifiedTLSBeforeContactingDatabricks(t *testing.T) {
	const endpoint = "projects/p/branches/b/endpoints/e"
	isolateDatabricksSDK(t)
	t.Run("single endpoint", func(t *testing.T) {
		t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
		t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpoint)
		requireUnverifiedTLSError(t, ConfigurePool(t.Context(), testPool(t, "require")))
	})
	t.Run("fleet mapping", func(t *testing.T) {
		t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
		t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"DB.example.":"`+endpoint+`"}`)
		requireUnverifiedTLSError(t, ConfigurePool(t.Context(), testPool(t, "verify-ca")))
	})
}

func TestNonOAuthHostsKeepTheirTLSMode(t *testing.T) {
	// An unmapped host is ordinary PostgreSQL: the verify-full requirement
	// applies only to connections that receive a minted OAuth credential.
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	for _, mapping := range []string{"", `{"other.example":"projects/p/branches/b/endpoints/e"}`} {
		t.Setenv("SAGE_LAKEBASE_ENDPOINTS", mapping)
		for _, ssl := range []string{"disable", "prefer"} {
			cfg := testPool(t, ssl)
			if err := ConfigurePool(t.Context(), cfg); err != nil {
				t.Fatalf("mapping %q sslmode=%s: %v", mapping, ssl, err)
			}
			if cfg.BeforeConnect != nil || cfg.ConnConfig.Password != "bootstrap" {
				t.Fatalf("mapping %q sslmode=%s: ordinary authentication changed", mapping, ssl)
			}
		}
	}
}

// No new concurrency surface: the TLS check is a pure function of the
// connection config; concurrent BeforeConnect is covered by the refresh test.
