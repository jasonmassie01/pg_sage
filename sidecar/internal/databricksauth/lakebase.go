package databricksauth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/lakebaseid"
)

// ErrUnverifiedTLS refuses a Lakebase OAuth connection whose TLS does not
// verify the endpoint's certificate and hostname. sslmode=require and
// verify-ca encrypt, but an interceptor could still capture the minted token.
// Fleet databases take sslmode from YAML (default prefer), so it is named too.
var ErrUnverifiedTLS = errors.New("lakebase OAuth requires sslmode=verify-full " +
	"(got an unverified TLS mode); set sslmode=verify-full in SAGE_DATABASE_URL, " +
	"SAGE_PG_SSLMODE or the fleet's databases[].sslmode")

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

// ErrEndpointPermanent marks a Lakebase setup error that retrying cannot fix:
// a configuration mistake, or an endpoint the principal may not read or that
// does not exist. Every other resolve error is transient and worth a retry.
var ErrEndpointPermanent = errors.New("lakebase endpoint error is not retryable")

type permanentError struct{ err error }

func (e permanentError) Error() string   { return e.err.Error() }
func (e permanentError) Unwrap() []error { return []error{ErrEndpointPermanent, e.err} }

func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// endpointAPI is the slice of the workspace Postgres service the pool hook
// uses; tests replace newEndpointAPI with a fake.
type endpointAPI interface {
	GetEndpoint(context.Context, postgres.GetEndpointRequest) (*postgres.Endpoint, error)
	GenerateDatabaseCredential(context.Context,
		postgres.GenerateDatabaseCredentialRequest) (*postgres.DatabaseCredential, error)
}

var newEndpointAPI = func() (endpointAPI, error) {
	w, err := workspace()
	if err != nil {
		return nil, err
	}
	return w.Postgres, nil
}

// ConfigurePool opts a pool into Lakebase OAuth with SAGE_LAKEBASE_ENDPOINT,
// or SAGE_LAKEBASE_ENDPOINTS (a JSON object mapping hostnames to endpoints) for
// fleet mode. Ordinary PostgreSQL connections retain their existing behavior.
// Environment changes require a process restart. Errors wrapping
// ErrEndpointPermanent will not succeed on retry; others may.
func ConfigurePool(ctx context.Context, cfg *pgxpool.Config) error {
	endpoint, err := endpointForHost(cfg.ConnConfig.Host)
	if err != nil || endpoint == "" {
		return permanent(err)
	}
	// Refuse before contacting Databricks: no credential is minted for a
	// connection that could not keep it private.
	if err := requireVerifiedTLS(cfg.ConnConfig); err != nil {
		return permanent(err)
	}
	api, err := newEndpointAPI()
	if err != nil {
		// workspace() caches its outcome for the process: a retry repeats it.
		return permanent(fmt.Errorf("configure Lakebase unified authentication: %w", err))
	}
	status, err := resolveEndpointStatus(ctx, api, endpoint)
	if err != nil {
		return err
	}
	mint := func(ctx context.Context) (*postgres.DatabaseCredential, error) {
		return api.GenerateDatabaseCredential(ctx,
			postgres.GenerateDatabaseCredentialRequest{Endpoint: endpoint})
	}
	if err := configureEndpointPool(cfg, status.Hosts, mint, trackedAs(endpoint)); err != nil {
		return permanent(err)
	}
	// databricks-04: keep the suspend timeout and state the same call
	// returned, only once the pool is configured, so a refusal stores nothing.
	recordEndpointInfo(endpoint, endpointInfoFrom(status, time.Now()))
	return nil
}

// endpointForHost returns the endpoint resource configured for host, or ""
// when host is ordinary PostgreSQL.
func endpointForHost(host string) (string, error) {
	return lakebaseid.EndpointForHost(os.Getenv, host)
}

// resolveEndpointStatus reads the endpoint's status, which always carries
// connection hosts. Permission and not-found errors are permanent; throttling,
// outages and network errors are left retryable for the caller's startup loop.
func resolveEndpointStatus(
	ctx context.Context, api endpointAPI, endpoint string,
) (*postgres.EndpointStatus, error) {
	ep, err := api.GetEndpoint(ctx, postgres.GetEndpointRequest{Name: endpoint})
	if err != nil {
		err = fmt.Errorf("resolve Lakebase endpoint %s: %w", endpoint, err)
		if errors.Is(err, apierr.ErrPermissionDenied) || errors.Is(err, apierr.ErrNotFound) {
			return nil, permanent(err)
		}
		return nil, err
	}
	if ep == nil || ep.Status == nil || ep.Status.Hosts == nil {
		return nil, fmt.Errorf("lakebase endpoint %s has no connection hosts", endpoint)
	}
	return ep.Status, nil
}

type mintCredential func(context.Context) (*postgres.DatabaseCredential, error)

func configureEndpointPool(
	cfg *pgxpool.Config, hosts *postgres.EndpointHosts, mint mintCredential, opts ...poolOption,
) error {
	if allowedHost(cfg.ConnConfig.Host, []string{hosts.ReadOnlyPooledHost, hosts.ReadWritePooledHost}) {
		return fmt.Errorf("lakebase OAuth does not support pooled endpoints; use the direct endpoint hostname")
	}
	return configurePool(cfg, []string{hosts.Host, hosts.ReadOnlyHost}, mint, opts...)
}

func configurePool(
	cfg *pgxpool.Config, allowed []string, mint mintCredential, opts ...poolOption,
) error {
	if err := validateConnection(cfg.ConnConfig, allowed); err != nil {
		return err
	}
	cache := &credentialCache{mint: mint}
	for _, opt := range opts {
		opt(cache)
	}
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
		token, err := cache.token(ctx)
		if err != nil {
			return err
		}
		conn.Password = token
		return nil
	}
	// No expired bootstrap password is retained in pool.Config().
	cfg.ConnConfig.Password = ""
	return nil
}

func validateConnection(conn *pgx.ConnConfig, allowed []string) error {
	if err := requireVerifiedTLS(conn); err != nil {
		return err
	}
	for _, fallback := range conn.Fallbacks {
		if !allowedHost(fallback.Host, allowed) {
			return fmt.Errorf(
				"lakebase OAuth fallback host does not match the configured Lakebase endpoint")
		}
	}
	if !allowedHost(conn.Host, allowed) {
		return fmt.Errorf("postgres host does not match the configured Lakebase endpoint")
	}
	return nil
}

// requireVerifiedTLS accepts only pgx's verify-full shape on the main host and
// on every fallback host.
func requireVerifiedTLS(conn *pgx.ConnConfig) error {
	if !verifiedTLS(conn.TLSConfig, conn.Host) {
		return ErrUnverifiedTLS
	}
	for _, fallback := range conn.Fallbacks {
		if !verifiedTLS(fallback.TLSConfig, fallback.Host) {
			return ErrUnverifiedTLS
		}
	}
	return nil
}

// verifiedTLS reports whether c verifies the certificate chain and binds it to
// host. pgx sets InsecureSkipVerify for require and verify-ca (verify-ca checks
// the chain in a callback but ignores the hostname), so only verify-full passes.
func verifiedTLS(c *tls.Config, host string) bool {
	return c != nil && !c.InsecureSkipVerify && strings.EqualFold(c.ServerName, host)
}

func allowedHost(host string, allowed []string) bool {
	for _, candidate := range allowed {
		if candidate != "" && strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(candidate, ".")) {
			return true
		}
	}
	return false
}
