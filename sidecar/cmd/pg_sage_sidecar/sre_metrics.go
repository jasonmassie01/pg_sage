package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/sre"
)

// observability-06: the Sage SRE investigation backlog. Each database
// whose investigator runs refreshes its own investigations from the
// coordination store (the meta database when there is one) on the
// refresher's cadence, so a growing queue, runs stuck in needs_evidence
// or a rising failure rate show on /metrics.

// metricsSourceSRE is the refresh source label of the backlog read.
const metricsSourceSRE = "sre"

// sreTerminalStates are the final investigation states (sre.State.Terminal).
const sreTerminalStates = "('concluded', 'inconclusive', 'cancelled', 'expired', 'failed')"

// sreBacklogSQL counts one binding's investigations by state: every live
// one, and terminal ones last updated in the past 24 hours. The third
// column is the age of the oldest live one in the server's clock (NULL
// when none is live). The (deployment_id, database_id, state, created_at)
// index serves it.
const sreBacklogSQL = `/* pg_sage */ SELECT i.state, count(*),
	extract(epoch FROM now() - min(i.created_at)
		FILTER (WHERE i.state NOT IN ` + sreTerminalStates + `))::float8
	FROM sage.sre_investigations i
	JOIN sage.sre_database_bindings b USING (deployment_id, database_id)
	WHERE b.runtime_key = $1
	  AND (i.state NOT IN ` + sreTerminalStates + `
	       OR i.updated_at > now() - interval '24 hours')
	GROUP BY i.state`

// sreBacklogLimitsSQL bounds the read on the server too: the control
// database may be the monitored one.
const sreBacklogLimitsSQL = `/* pg_sage */ SELECT
	set_config('statement_timeout', '2s', true), set_config('lock_timeout', '1s', true)`

// sreBacklog is one database's investigations as last refreshed.
type sreBacklog struct {
	byState map[string]int64
	// oldestOpen is when the oldest live investigation was created, in
	// the sidecar's clock (zero: none is live).
	oldestOpen time.Time
}

// sreBacklogReader reads one database's backlog.
type sreBacklogReader func(ctx context.Context) (sreBacklog, error)

// controlBacklogReader reads the backlog of the binding runtimeKey names
// from the coordination store p.
func controlBacklogReader(p *pgxpool.Pool, runtimeKey string) sreBacklogReader {
	return func(ctx context.Context) (sreBacklog, error) {
		return readSREBacklog(ctx, p, runtimeKey)
	}
}

// sreMetricsSource refreshes the backlog. A failed read keeps the previous
// sample, so one slow refresh does not blank the backlog series and flap
// an alert on them; pg_sage_metrics_refresh_success{source="sre"} shows it.
func sreMetricsSource(read sreBacklogReader) metricsSource {
	return metricsSource{name: metricsSourceSRE,
		read: func(ctx context.Context, s *metricsSnapshot) error {
			b, err := read(ctx)
			if err != nil {
				return fmt.Errorf("count Sage SRE investigations: %w", err)
			}
			s.sre = &b
			return nil
		},
		carry: func(prev, next *metricsSnapshot) { next.sre = prev.sre }}
}

// readSREBacklog runs the backlog query in a read-only transaction under
// statement and lock timeouts.
func readSREBacklog(ctx context.Context, p *pgxpool.Pool, runtimeKey string) (
	sreBacklog, error) {
	if p == nil {
		return sreBacklog{}, errNoMetricsPool
	}
	out := sreBacklog{byState: map[string]int64{}}
	err := pgx.BeginTxFunc(ctx, p, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, sreBacklogLimitsSQL); err != nil {
				return fmt.Errorf("set read timeouts: %w", err)
			}
			return scanSREBacklog(ctx, tx, runtimeKey, &out)
		})
	if err != nil {
		return sreBacklog{}, err
	}
	return out, nil
}

func scanSREBacklog(ctx context.Context, tx pgx.Tx, runtimeKey string,
	out *sreBacklog) error {
	rows, err := tx.Query(ctx, sreBacklogSQL, runtimeKey)
	if err != nil {
		return fmt.Errorf("read sage.sre_investigations: %w", err)
	}
	defer rows.Close()
	now := time.Now()
	oldestAge := -1.0
	for rows.Next() {
		var state string
		var n int64
		var age *float64
		if err := rows.Scan(&state, &n, &age); err != nil {
			return fmt.Errorf("scan sage.sre_investigations counts: %w", err)
		}
		out.byState[state] = n
		if age != nil && *age > oldestAge {
			oldestAge = *age
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sage.sre_investigations: %w", err)
	}
	if oldestAge >= 0 {
		out.oldestOpen = now.Add(-time.Duration(oldestAge * float64(time.Second)))
	}
	return nil
}

// writeSREInvestigationMetrics exports each database's backlog: counts by
// state (every live state, zero included; terminal states only when
// counted in the past 24 hours) and the age of the oldest live
// investigation (0 when none is live). Nothing, not even HELP, is written
// when no database runs Sage SRE.
func writeSREInvestigationMetrics(b *strings.Builder, views []metricsView) {
	var counts, ages strings.Builder
	for _, v := range views {
		if v.snap == nil || v.snap.sre == nil {
			continue
		}
		bl := v.snap.sre
		for _, st := range sre.AllStates() {
			n, counted := bl.byState[string(st)]
			if !counted && st.Terminal() {
				continue
			}
			writeSample(&counts, "pg_sage_sre_investigations",
				withLabel(dbLabel(v.name), "state", string(st)), n)
		}
		age := 0.0
		if !bl.oldestOpen.IsZero() {
			age = max(0, time.Since(bl.oldestOpen).Seconds())
		}
		writeSample(&ages, "pg_sage_sre_investigation_oldest_open_age_seconds",
			dbLabel(v.name), formatGauge(age))
	}
	if ages.Len() == 0 {
		return
	}
	metricsreg.Header(b, "pg_sage_sre_investigations")
	b.WriteString(counts.String())
	metricsreg.Header(b, "pg_sage_sre_investigation_oldest_open_age_seconds")
	b.WriteString(ages.String() + "\n")
}
