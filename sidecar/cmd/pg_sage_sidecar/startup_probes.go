package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

func detectCloudEnvironment() string {
	return detectCloudEnv(pool)
}

// detectCloudEnv probes a connection pool for managed service
// indicators. Pass any per-database pool in fleet mode. A probe that keeps
// erroring reads as unknown, never as self-managed. A YAML fleet has no
// process pool, so nil is unknown without a probe-failure warning.
func detectCloudEnv(p *pgxpool.Pool) string {
	if p == nil {
		return providerUnknown
	}
	provider, err := detectCloudEnvContext(context.Background(), p)
	if err != nil {
		logWarn("startup", "provider probe failed; provider %s: %v", provider, err)
	}
	return provider
}

// detectRuntimeProvider is the provider detection a database runtime uses;
// tests replace it to simulate an outage.
var detectRuntimeProvider = detectCloudEnvContext

// detectCloudEnvContext names the provider from the host, or else from the
// SQL probes, retried with backoff at startup. It returns unknown with the
// last error when the probes never succeeded.
func detectCloudEnvContext(ctx context.Context, p *pgxpool.Pool) (string, error) {
	if p == nil {
		return providerUnknown, errors.New("provider probe: no connection pool")
	}
	if provider := hostedProviderFromHost(p.Config().ConnConfig.Host); provider != "" {
		return provider, nil
	}
	return newProviderDetector(p).detect(ctx)
}

func poolHealthCheck() {
	if pool == nil {
		return // fleet mode: no global pool to health-check
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(shutdownCtx, 5*time.Second)
			if err := pool.Ping(ctx); err != nil {
				logWarn("pool-health", "ping failed: %v", err)
			}
			cancel()
			stat := pool.Stat()
			if stat.TotalConns() == stat.MaxConns() && stat.IdleConns() == 0 {
				logWarn("pool-health", "exhausted — total=%d max=%d",
					stat.TotalConns(), stat.MaxConns())
			}
		case <-shutdownCtx.Done():
			return
		}
	}
}

// bootstrapAdminIfEmpty creates the first admin user when no users
// exist. Only generated credentials are printed to stderr.
func bootstrapAdminIfEmpty(
	ctx context.Context, p *pgxpool.Pool,
) error {
	appAuth, err := auth.LoadDatabricksAppConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if appAuth != nil {
		return auth.BootstrapDatabricksAppAdmin(ctx, p, *appAuth)
	}
	count, err := auth.UserCount(ctx, p)
	if err != nil {
		return fmt.Errorf("checking user count: %w", err)
	}
	if count > 0 {
		return nil
	}
	credentials, err := loadBootstrapAdminCredentials()
	if err != nil {
		return fmt.Errorf("loading admin bootstrap credentials: %w", err)
	}
	if err := auth.BootstrapAdmin(
		ctx, p, credentials.email, credentials.password,
	); err != nil {
		return fmt.Errorf("creating admin: %w", err)
	}
	if credentials.supplied {
		logInfo("startup", "first admin created — email: %s (configured bootstrap secret; password not logged)", credentials.email)
		return nil
	}
	logInfo("startup",
		"first admin created — email: %s  password: [redacted, see stderr]",
		credentials.email)
	fmt.Fprintf(os.Stderr, "\n*** INITIAL ADMIN PASSWORD: %s ***\n"+
		"*** Change this password immediately. ***\n\n", credentials.password)
	return nil
}

// parseConfigRampStart parses cfg.Trust.RampStart (accepted in
// RFC3339 or date-only form) into a time.Time. Returns the zero
// time on empty input or unparseable values — callers may use
// IsZero() to distinguish "not set" from a valid timestamp.
func parseConfigRampStart(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339, "2006-01-02", "2006-01-02T15:04:05",
	} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
