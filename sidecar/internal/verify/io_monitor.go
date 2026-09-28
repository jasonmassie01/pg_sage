package verify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// maxRateAge bounds how old the latest rate may be and still describe
// "now": two sampling intervals plus scheduling slack.
const maxRateAge = 150 * time.Second

// IOEvidence is the pg-side part of load evidence for one database.
type IOEvidence struct {
	Rate      *IORate
	RateError string
	Baseline  IOBaseline
}

// IOMonitor samples pg-side IO rates for one database, persists them in
// sage.io_rate_sample and derives the rolling baseline. Sampling is
// serialized, so concurrent callers cannot record overlapping intervals.
type IOMonitor struct {
	pool      *pgxpool.Pool
	database  string
	retention time.Duration
	now       func() time.Time

	mu        sync.Mutex
	previous  *IOCounters
	latest    *IORate
	latestErr string
}

// NewIOMonitor builds a monitor; retention bounds the stored samples.
func NewIOMonitor(pool *pgxpool.Pool, database string, retention time.Duration) *IOMonitor {
	return &IOMonitor{pool: pool, database: database, retention: retention, now: time.Now}
}

// Run samples every interval until ctx ends; report receives sample errors.
func (m *IOMonitor) Run(ctx context.Context, interval time.Duration, report func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := m.Sample(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sample reads the counters and, when a previous reading exists, persists
// the rate over the interval. A reset or out-of-range interval clears the
// current rate: it is missing evidence, never a quiet period.
func (m *IOMonitor) Sample(ctx context.Context) error {
	if m == nil || m.pool == nil {
		return errors.New("IO sampler has no database pool")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := ReadIOCounters(ctx, m.pool)
	if err != nil {
		m.latest, m.latestErr = nil, err.Error()
		return err
	}
	current.At = m.now()
	if m.previous == nil {
		m.previous, m.latestErr = &current, "IO sampler is priming its first interval"
		return nil
	}
	rate, err := DeriveIORate(*m.previous, current)
	if errors.Is(err, ErrSampleTooSoon) {
		return err
	}
	m.previous = &current
	if err != nil {
		m.latest, m.latestErr = nil, err.Error()
		return err
	}
	if err := m.persist(ctx, rate); err != nil {
		m.latest, m.latestErr = nil, err.Error()
		return err
	}
	m.latest, m.latestErr = &rate, ""
	return nil
}

func (m *IOMonitor) persist(ctx context.Context, rate IORate) error {
	if _, err := m.pool.Exec(ctx, `INSERT INTO sage.io_rate_sample
		(database_name, sampled_at, interval_seconds, data_bytes_per_sec,
		 wal_bytes_per_sec, source) VALUES ($1, $2, $3, $4, $5, $6)`,
		m.database, rate.At, rate.Interval.Seconds(), rate.DataBytesPerSec,
		rate.WALBytesPerSec, rate.Source); err != nil {
		return fmt.Errorf("persist IO rate: %w", err)
	}
	if _, err := m.pool.Exec(ctx, `DELETE FROM sage.io_rate_sample
		WHERE database_name = $1 AND sampled_at < $2`,
		m.database, rate.At.Add(-m.retention)); err != nil {
		return fmt.Errorf("prune IO rates: %w", err)
	}
	return nil
}

// IOEvidence returns the freshest rate and the rolling baseline.
func (m *IOMonitor) IOEvidence(ctx context.Context) (IOEvidence, error) {
	if m == nil || m.pool == nil {
		return IOEvidence{}, errors.New("IO sampler has no database pool")
	}
	now := m.now()
	samples, err := m.recentRates(ctx, now.Add(-m.retention))
	if err != nil {
		return IOEvidence{}, err
	}
	evidence := IOEvidence{Baseline: ComputeIOBaseline(samples)}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.latest == nil && m.latestErr == "":
		evidence.RateError = "IO sampler has not sampled yet"
	case m.latest == nil:
		evidence.RateError = m.latestErr
	case now.Sub(m.latest.At) > maxRateAge:
		evidence.RateError = fmt.Sprintf("latest IO rate is stale (%s old)",
			now.Sub(m.latest.At).Round(time.Second))
	default:
		latest := *m.latest
		evidence.Rate = &latest
	}
	return evidence, nil
}

func (m *IOMonitor) recentRates(ctx context.Context, since time.Time) ([]IORate, error) {
	rows, err := m.pool.Query(ctx, `SELECT sampled_at, interval_seconds,
		data_bytes_per_sec, wal_bytes_per_sec, source FROM sage.io_rate_sample
		WHERE database_name = $1 AND sampled_at >= $2`, m.database, since)
	if err != nil {
		return nil, fmt.Errorf("read IO baseline: %w", err)
	}
	defer rows.Close()
	var rates []IORate
	for rows.Next() {
		var rate IORate
		var seconds float64
		if err := rows.Scan(&rate.At, &seconds, &rate.DataBytesPerSec,
			&rate.WALBytesPerSec, &rate.Source); err != nil {
			return nil, fmt.Errorf("scan IO baseline: %w", err)
		}
		rate.Interval = time.Duration(seconds * float64(time.Second))
		rates = append(rates, rate)
	}
	return rates, rows.Err()
}
