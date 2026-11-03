package databricksauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var endpointName = regexp.MustCompile(`^projects/[a-z0-9-]+/branches/[a-z0-9-]+/endpoints/[a-z0-9-]+$`)

// NewPool is the OAuth-aware equivalent of pgxpool.New for CLI commands.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if err := ConfigurePool(ctx, cfg); err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// ConfigureConnection obtains a credential for a single physical connection
// using the same endpoint validation as a monitoring pool. Callers must retain
// the logical endpoint hostname when pinning a resolved network address.
func ConfigureConnection(ctx context.Context, conn *pgx.ConnConfig) error {
	cfg := &pgxpool.Config{ConnConfig: conn}
	if err := ConfigurePool(ctx, cfg); err != nil {
		return err
	}
	if cfg.BeforeConnect != nil {
		return cfg.BeforeConnect(ctx, conn)
	}
	return nil
}

// ConfigurePool opts a pool into Lakebase OAuth with SAGE_LAKEBASE_ENDPOINT,
// or SAGE_LAKEBASE_ENDPOINTS (a JSON object mapping hostnames to endpoints) for
// fleet mode. Ordinary PostgreSQL connections retain their existing behavior.
// Environment changes require a process restart.
func ConfigurePool(ctx context.Context, cfg *pgxpool.Config) error {
	endpoint := os.Getenv("SAGE_LAKEBASE_ENDPOINT")
	if raw := os.Getenv("SAGE_LAKEBASE_ENDPOINTS"); raw != "" {
		if endpoint != "" {
			return fmt.Errorf("set only one of SAGE_LAKEBASE_ENDPOINT and SAGE_LAKEBASE_ENDPOINTS")
		}
		var endpoints map[string]string
		if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
			return fmt.Errorf("SAGE_LAKEBASE_ENDPOINTS must be a JSON object mapping hostnames to endpoint resource paths")
		}
		for host, resource := range endpoints {
			if !endpointName.MatchString(resource) {
				return fmt.Errorf("invalid Lakebase endpoint resource path in SAGE_LAKEBASE_ENDPOINTS")
			}
			if strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(cfg.ConnConfig.Host, ".")) {
				endpoint = resource
			}
		}
	}
	if endpoint == "" {
		return nil
	}
	if !endpointName.MatchString(endpoint) {
		return fmt.Errorf("lakebase endpoint must be projects/<id>/branches/<id>/endpoints/<id>")
	}
	w, err := workspace()
	if err != nil {
		return fmt.Errorf("configure Lakebase unified authentication: %w", err)
	}
	ep, err := w.Postgres.GetEndpoint(ctx, postgres.GetEndpointRequest{Name: endpoint})
	if err != nil {
		return fmt.Errorf("resolve Lakebase endpoint: %w", err)
	}
	if ep.Status == nil || ep.Status.Hosts == nil {
		return fmt.Errorf("lakebase endpoint has no connection hosts")
	}
	hosts := ep.Status.Hosts
	return configureEndpointPool(cfg, hosts, func(ctx context.Context) (*postgres.DatabaseCredential, error) {
		return w.Postgres.GenerateDatabaseCredential(ctx, postgres.GenerateDatabaseCredentialRequest{Endpoint: endpoint})
	})
}

type mintCredential func(context.Context) (*postgres.DatabaseCredential, error)

func configureEndpointPool(cfg *pgxpool.Config, hosts *postgres.EndpointHosts, mint mintCredential) error {
	if allowedHost(cfg.ConnConfig.Host, []string{hosts.ReadOnlyPooledHost, hosts.ReadWritePooledHost}) {
		return fmt.Errorf("lakebase OAuth does not support pooled endpoints; use the direct endpoint hostname")
	}
	return configurePool(cfg, []string{hosts.Host, hosts.ReadOnlyHost}, mint)
}

func configurePool(cfg *pgxpool.Config, allowed []string, mint mintCredential) error {
	if err := validateConnection(cfg.ConnConfig, allowed); err != nil {
		return err
	}
	var mu sync.Mutex
	var cached *postgres.DatabaseCredential
	previous := cfg.BeforeConnect
	cfg.BeforeConnect = func(ctx context.Context, conn *pgx.ConnConfig) error {
		if previous != nil {
			if err := previous(ctx, conn); err != nil {
				return err
			}
		}
		if err := validateConnection(conn, allowed); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if cached == nil || cached.ExpireTime == nil || time.Until(cached.ExpireTime.AsTime()) < 2*time.Minute {
			next, err := mint(ctx)
			if err != nil {
				return fmt.Errorf("refresh Lakebase database credential: %w", err)
			}
			if next == nil || next.Token == "" || next.ExpireTime == nil || time.Until(next.ExpireTime.AsTime()) <= 0 {
				return fmt.Errorf("lakebase returned an empty or expired database credential")
			}
			cached = next
		}
		conn.Password = cached.Token
		return nil
	}
	// No expired bootstrap password is retained in pool.Config().
	cfg.ConnConfig.Password = ""
	return nil
}

func validateConnection(conn *pgx.ConnConfig, allowed []string) error {
	if conn.TLSConfig == nil {
		return fmt.Errorf("lakebase OAuth requires sslmode=require or verify-full")
	}
	for _, fallback := range conn.Fallbacks {
		if fallback.TLSConfig == nil || !allowedHost(fallback.Host, allowed) {
			return fmt.Errorf("lakebase OAuth does not allow plaintext or unverified fallback hosts")
		}
	}
	if !allowedHost(conn.Host, allowed) {
		return fmt.Errorf("postgres host does not match the configured Lakebase endpoint")
	}
	return nil
}

func allowedHost(host string, allowed []string) bool {
	for _, candidate := range allowed {
		if candidate != "" && strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(candidate, ".")) {
			return true
		}
	}
	return false
}
