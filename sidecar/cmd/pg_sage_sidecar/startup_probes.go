package main

import (
	"context"
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
// indicators. Pass any per-database pool in fleet mode.
func detectCloudEnv(p *pgxpool.Pool) string {
	if p == nil {
		return "unknown"
	}
	if provider := hostedProviderFromHost(p.Config().ConnConfig.Host); provider != "" {
		return provider
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var s string
	if p.QueryRow(ctx, "SELECT aurora_version()").Scan(&s) == nil {
		return "aurora"
	}
	var ps *string
	if p.QueryRow(ctx, "SELECT current_setting('rds.extensions', true)").Scan(&ps) == nil && ps != nil {
		return "rds"
	}
	if p.QueryRow(ctx, "SELECT current_setting('alloydb.iam_authentication', true)").Scan(&ps) == nil && ps != nil {
		return "alloydb"
	}
	if p.QueryRow(ctx, "SELECT current_setting('cloudsql.iam_authentication', true)").Scan(&ps) == nil && ps != nil {
		return "cloud-sql"
	}
	if p.QueryRow(ctx, "SELECT current_setting('azure.extensions', true)").Scan(&ps) == nil && ps != nil {
		return "azure"
	}
	return "self-managed"
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
// exist. Prints credentials to stdout so the operator can log in.
func bootstrapAdminIfEmpty(
	ctx context.Context, p *pgxpool.Pool,
) error {
	count, err := auth.UserCount(ctx, p)
	if err != nil {
		return fmt.Errorf("checking user count: %w", err)
	}
	if count > 0 {
		return nil
	}
	password, err := generateRandomPassword(adminPassLen)
	if err != nil {
		return fmt.Errorf("generating admin password: %w", err)
	}
	if err := auth.BootstrapAdmin(
		ctx, p, adminEmail, password,
	); err != nil {
		return fmt.Errorf("creating admin: %w", err)
	}
	logInfo("startup",
		"first admin created — email: %s  password: [redacted, see stderr]",
		adminEmail)
	fmt.Fprintf(os.Stderr, "\n*** INITIAL ADMIN PASSWORD: %s ***\n*** Change this password immediately. ***\n\n", password)
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
