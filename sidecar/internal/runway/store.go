package runway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/sre"
)

// pruneBatch bounds the samples one tick deletes.
const pruneBatch = 10000

// pruneSamplesSQL and loadLastSQL bound sampled_at with now(), which is
// stable within the statement, so runway_samples_sampled_at_idx serves
// them; a volatile clock_timestamp() cutoff cannot be an index bound and
// read the whole table every pass (performance gate). The prune collects
// its batch's ids into an array: id IN (subquery) may be planned as a hash
// join over a full scan of the table.
const (
	pruneSamplesSQL = `/* pg_sage */ DELETE FROM sage.runway_samples
		WHERE id = ANY (ARRAY(SELECT id FROM sage.runway_samples
		             WHERE sampled_at < now() - make_interval(secs => $1)
		             LIMIT $2))`
	loadLastSQL = `/* pg_sage */
		SELECT DISTINCT ON (kind, subject) kind, subject, epoch, counter
		FROM sage.runway_samples
		WHERE sampled_at > now() - make_interval(secs => $1)
		ORDER BY kind, subject, sampled_at DESC`
)

// sample writes the snapshot's samples, each in its series' epoch, then
// prunes samples past the retention.
func (m *Monitor) sample(ctx context.Context, snap Snapshot) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		if err := m.loadLast(ctx); err != nil {
			return 0, fmt.Errorf("read the last runway samples: %w", err)
		}
		m.loaded = true
	}
	now := time.Now()
	ss := assignEpochs(BuildSamples(snap, m.opts), m.last, now)
	rows := make([][]any, 0, len(ss))
	for _, s := range ss {
		rows = append(rows, []any{s.Kind, s.Subject, s.Epoch, now, s.Value,
			nullable(s.Counter), nullable(s.Limit)})
	}
	if len(rows) > 0 {
		if _, err := m.pool.CopyFrom(ctx, pgx.Identifier{"sage", "runway_samples"},
			[]string{"kind", "subject", "epoch", "sampled_at", "value", "counter",
				"limit_value"}, pgx.CopyFromRows(rows)); err != nil {
			m.loaded = false // the epochs advanced in memory were not stored
			return 0, fmt.Errorf("write %d runway samples: %w", len(rows), err)
		}
	}
	if _, err := m.pool.Exec(ctx, pruneSamplesSQL, m.opts.Retention.Seconds(),
		pruneBatch); err != nil {
		return len(rows), fmt.Errorf("prune runway samples: %w", err)
	}
	return len(rows), nil
}

func nullable(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

// loadLast reads each series' latest epoch and counter, so a restart
// continues the series instead of starting new epochs.
func (m *Monitor) loadLast(ctx context.Context) error {
	rows, err := m.pool.Query(ctx, loadLastSQL, m.opts.Retention.Seconds())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k seriesKey
		var p lastPoint
		var counter *float64
		if err := rows.Scan(&k.kind, &k.subject, &p.epoch, &counter); err != nil {
			return err
		}
		p.counter = math.NaN()
		if counter != nil {
			p.counter = *counter
		}
		m.last[k] = p
	}
	return rows.Err()
}

// report upserts the runways' findings, resolves the cleared findings of
// the evaluated categories and starts the investigations.
func (m *Monitor) report(ctx context.Context, runways []Runway,
	evaluated []string) (int, []string, error) {
	findings := make([]analyzer.Finding, 0, len(runways))
	active := map[string]map[string]bool{}
	for _, r := range runways {
		findings = append(findings, r.Finding())
		if active[r.Category] == nil {
			active[r.Category] = map[string]bool{}
		}
		active[r.Category][r.Identifier] = true
	}
	if _, err := analyzer.UpsertFindingsWithResult(ctx, m.pool, findings); err != nil {
		return 0, nil, fmt.Errorf("record runway findings: %w", err)
	}
	var resolved []string
	var errs []error
	for _, cat := range evaluated {
		if err := analyzer.ResolveCleared(ctx, m.pool, active[cat], cat); err != nil {
			errs = append(errs, fmt.Errorf("resolve cleared %s findings: %w", cat, err))
			continue
		}
		resolved = append(resolved, cat)
	}
	started := 0
	if m.opts.Investigate && m.starter != nil {
		for _, r := range runways {
			if m.start(ctx, r) {
				started++
			}
		}
	}
	return started, resolved, errors.Join(errs...)
}

// start opens (or coalesces into) the runway's investigation, keyed by
// its open finding and severity. A failure is logged, not fatal.
func (m *Monitor) start(ctx context.Context, r Runway) bool {
	var id int64
	err := m.pool.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'`,
		r.Category, r.Identifier).Scan(&id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) { // no row: suppressed by an operator
			m.logFn("WARN", "runway: db %q: find the %s finding of %s: %v",
				m.opts.Database, r.Category, r.Identifier, err)
		}
		return false
	}
	caseID := cases.IdentityKeyForFinding(cases.SourceFinding{DatabaseName: m.opts.Database,
		Category: r.Category, ObjectType: r.ObjectType, ObjectIdentifier: r.Identifier})
	_, _, err = m.starter.Start(ctx, sre.Trigger{CaseID: clip(caseID, 256), Kind: r.Kind,
		Subject: clip(r.Subject, 256), IdempotencyKey: fmt.Sprintf("runway:%d:%s", id,
			r.Severity)})
	if err != nil {
		m.logFn("WARN", "runway: db %q: start the pre-incident investigation of %s: %v",
			m.opts.Database, r.Subject, err)
		return false
	}
	return true
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
