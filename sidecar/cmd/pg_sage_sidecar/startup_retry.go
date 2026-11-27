package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/databricksauth"
)

const (
	startupAttempts = 5
	// startupResolveTimeout bounds one attempt's Lakebase endpoint lookup;
	// the SDK alone would retry a throttled control plane for minutes.
	startupResolveTimeout = 30 * time.Second
	startupPingTimeout    = 5 * time.Second
)

var (
	// startupBackoff is the first retry delay; it doubles per attempt
	// (1s, 2s, 4s, 8s). Tests shrink it.
	startupBackoff        = time.Second
	configureLakebasePool = databricksauth.ConfigurePool
)

// retryStartup runs open up to attempts times with exponential backoff. It
// stops at once on a permanent Lakebase endpoint error (a misconfiguration,
// 403 or 404) and on ctx cancellation. A single attempt returns open's error
// as is.
func retryStartup(
	ctx context.Context, component string, attempts int,
	open func(context.Context) (*pgxpool.Pool, error),
) (*pgxpool.Pool, error) {
	backoff := startupBackoff
	var lastErr error
	for attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := open(ctx)
		if err == nil {
			return p, nil
		}
		if errors.Is(err, databricksauth.ErrEndpointPermanent) {
			return nil, err
		}
		lastErr = err
		if attempt < attempts-1 {
			logRetry(component, attempt, attempts, backoff, lastErr)
			if err := waitForRetry(ctx, backoff); err != nil {
				return nil, err
			}
			backoff *= 2
		}
	}
	if attempts == 1 {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%s failed after %d attempts: %w",
		component, attempts, lastErr)
}

// openStartupPool is one startup attempt: it copies base (Lakebase OAuth
// installs a hook and clears the password, so a config is never reused),
// resolves the endpoint under a bounded context, applies tune, then creates
// and pings the pool.
func openStartupPool(
	ctx context.Context, base *pgxpool.Config, tune func(*pgxpool.Config),
) (*pgxpool.Pool, error) {
	poolCfg := base.Copy()
	resolveCtx, cancel := context.WithTimeout(ctx, startupResolveTimeout)
	err := configureLakebasePool(resolveCtx, poolCfg)
	cancel()
	if err != nil {
		return nil, err
	}
	tune(poolCfg)
	// The pool keeps ctx for its background minimum connections, so it is
	// the caller's context, not the per-attempt one.
	p, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, startupPingTimeout)
	defer cancel()
	if err := p.Ping(pingCtx); err != nil {
		p.Close()
		return nil, fmt.Errorf("cannot connect: %w", err)
	}
	return p, nil
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logRetry(
	component string, attempt, max int,
	backoff time.Duration, err error,
) {
	logWarn(component,
		"attempt %d/%d failed: %v — retrying in %s",
		attempt+1, max, err, backoff)
}
