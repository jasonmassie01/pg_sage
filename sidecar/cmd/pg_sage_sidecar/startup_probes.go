package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

func detectCloudEnvironment() string {
	provider := detectCloudEnv(pool)
	if cfg != nil {
		provider, _ = cnpgProvider(provider, cfg.Postgres.CNPG) // the runtime warns
	}
	return provider
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

// poolHealthCheck pings the process pool at once and every 30 s; the last
// result is what pg_sage_connection_up reports (observability-03).
func poolHealthCheck() {
	if pool == nil {
		return // fleet mode: no global pool to health-check
	}
	checkPoolHealth(shutdownCtx, pool)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			checkPoolHealth(shutdownCtx, pool)
		case <-shutdownCtx.Done():
			return
		}
	}
}

// checkPoolHealth pings p under a 5 s bound, records the result in
// poolHealth and warns on a failed ping or an exhausted pool.
func checkPoolHealth(parent context.Context, p *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	err := p.Ping(ctx)
	cancel()
	if errors.Is(parent.Err(), context.Canceled) {
		return // shutting down: not a health result
	}
	poolHealth.record(err == nil)
	if err != nil {
		logWarn("pool-health", "ping failed: %v", err)
	}
	stat := p.Stat()
	if stat.TotalConns() == stat.MaxConns() && stat.IdleConns() == 0 {
		logWarn("pool-health", "exhausted — total=%d max=%d",
			stat.TotalConns(), stat.MaxConns())
	}
}

// healthState is the last pool health result: unknown until the first
// check, then up or down. Safe for concurrent use.
type healthState struct{ v atomic.Int32 }

const (
	healthUnknown int32 = iota
	healthUp
	healthDown
)

// poolHealth is the process pool's last health check.
var poolHealth healthState

func (h *healthState) record(up bool) {
	if up {
		h.v.Store(healthUp)
	} else {
		h.v.Store(healthDown)
	}
}

// state is 1 (up) or 0 (down), and false while no check has finished.
func (h *healthState) state() (int, bool) {
	switch h.v.Load() {
	case healthUp:
		return 1, true
	case healthDown:
		return 0, true
	}
	return 0, false
}

func (h *healthState) load() int32     { return h.v.Load() }
func (h *healthState) restore(v int32) { h.v.Store(v) }
func (h *healthState) reset()          { h.v.Store(healthUnknown) }

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
	if err := createBootstrapAdmin(ctx, p, credentials); err != nil {
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
		"*** You must change this password at first sign-in. ***\n\n", credentials.password)
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
