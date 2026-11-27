package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// providerUnknown is the answer when a probe errored: it matches no
	// action's provider support list and is not a managed service, so
	// provider-gated actions wait for a probe that succeeds.
	providerUnknown     = "unknown"
	providerSelfManaged = "self-managed"
	// providerProbeTimeout bounds each probe statement on its own.
	providerProbeTimeout = 2 * time.Second
	// sqlStateUndefinedFunction is how a server without aurora_version()
	// says it is not Aurora.
	sqlStateUndefinedFunction = "42883"
)

// providerStartupBackoff is the wait before each startup retry.
var providerStartupBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// providerQuerier is the part of a pool the provider probes use.
type providerQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// providerProbe identifies one provider by a function or setting that only
// it has. The list and its order are a compatibility contract. A statement
// whose value is one of also names that provider instead: the last one
// carries the Neon and Supabase fallbacks for custom domains, so detection
// stays at five statements.
type providerProbe struct {
	provider string
	sql      string
	also     []string
}

var providerProbes = []providerProbe{
	{provider: "aurora", sql: "SELECT aurora_version()"},
	{provider: "rds", sql: "SELECT current_setting('rds.extensions', true)"},
	{provider: "alloydb", sql: "SELECT current_setting('alloydb.iam_authentication', true)"},
	{provider: "cloud-sql", sql: "SELECT current_setting('cloudsql.iam_authentication', true)"},
	{provider: "azure", sql: azureAndFallbackProbe, also: []string{"neon", "supabase"}},
}

// azureAndFallbackProbe answers azure first, then a neon.* setting (Neon
// sets its tenant and timeline), then the supabase_admin role Supabase
// creates. pg_roles is readable by every role.
const azureAndFallbackProbe = `SELECT CASE
 WHEN current_setting('azure.extensions', true) IS NOT NULL THEN 'azure'
 WHEN COALESCE(NULLIF(current_setting('neon.tenant_id', true), ''),
   NULLIF(current_setting('neon.timeline_id', true), '')) IS NOT NULL THEN 'neon'
 WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'supabase_admin')
   THEN 'supabase'
 END`

// providerDetector runs the provider probes, with startup retries.
type providerDetector struct {
	query   providerQuerier
	timeout time.Duration
	backoff []time.Duration
	sleep   func(context.Context, time.Duration) error
}

func newProviderDetector(q providerQuerier) providerDetector {
	return providerDetector{query: q, timeout: providerProbeTimeout,
		backoff: providerStartupBackoff, sleep: sleepContext}
}

// detect probes, retrying after each backoff step, and settles on unknown
// (with the last error) when every attempt errored.
func (d providerDetector) detect(ctx context.Context) (string, error) {
	provider, err := d.probeOnce(ctx)
	for _, wait := range d.backoff {
		if err == nil {
			return provider, nil
		}
		if sleepErr := d.sleep(ctx, wait); sleepErr != nil {
			return providerUnknown, errors.Join(err, sleepErr)
		}
		provider, err = d.probeOnce(ctx)
	}
	return provider, err
}

// probeOnce runs every probe in order. The first positive answer names the
// provider; no positive answer is self-managed; any probe error is unknown.
func (d providerDetector) probeOnce(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return providerUnknown, err
	}
	for _, probe := range providerProbes {
		found, err := d.run(ctx, probe)
		if err != nil {
			return providerUnknown, fmt.Errorf("provider probe %s: %w", probe.provider, err)
		}
		if found != "" {
			return found, nil
		}
	}
	return providerSelfManaged, nil
}

// run names the provider the probe found, or "" for none. Only "function
// does not exist" and a NULL value mean "not this provider"; every other
// error (connection, timeout, 57014, permission) is returned.
func (d providerDetector) run(parent context.Context, probe providerProbe) (string, error) {
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()
	var value *string
	err := d.query.QueryRow(ctx, probe.sql).Scan(&value)
	if err == nil {
		switch {
		case value == nil:
			return "", nil
		case slices.Contains(probe.also, *value):
			return *value, nil
		}
		return probe.provider, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlStateUndefinedFunction {
		return "", nil
	}
	return "", err
}

// sleepContext waits for d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
