package slo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Store persists SLI samples and SLO state in the coordination database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore binds a store to the coordination pool.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: SLO store needs a pool", sre.ErrInvalidRequest)
	}
	return &Store{pool: pool}, nil
}

// Key identifies the database behind the store: stores sharing a pool
// share their rows.
func (s *Store) Key() *pgxpool.Pool { return s.pool }

// ErrInvalidSample reports a sample the store refuses.
var ErrInvalidSample = errors.New("invalid SLI sample")

// DefaultSeries names the series of a push that names none.
const DefaultSeries = "default"

var seriesPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// PushSample is one cumulative counter observation of one series (an
// application instance): bad and eligible events since the counter
// started. A decrease is a counter reset.
type PushSample struct {
	Series     string    `json:"series"`
	Bad        float64   `json:"bad"`
	Eligible   float64   `json:"eligible"`
	ObservedAt time.Time `json:"observed_at"`
}

func (p PushSample) validate() error {
	switch {
	case !seriesPattern.MatchString(p.Series):
		return fmt.Errorf("%w: series must match %s", ErrInvalidSample, seriesPattern)
	case p.ObservedAt.IsZero():
		return fmt.Errorf("%w: observed_at is required", ErrInvalidSample)
	case !finite(p.Bad) || !finite(p.Eligible) || p.Bad < 0 || p.Eligible < 0:
		return fmt.Errorf("%w: counters must be finite and non-negative", ErrInvalidSample)
	case p.Bad > p.Eligible:
		return fmt.Errorf("%w: bad exceeds eligible", ErrInvalidSample)
	}
	return nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// RecordSample stores one sample; a replay (same series and time) is a
// no-op with created false. value is an optional gauge (a proxy's
// measured value, e.g. interval p95 latency). The sample carries its
// series' running counters (store_cumulative.go).
func (s *Store) RecordSample(ctx context.Context, dep sre.UUID, slo string, p PushSample,
	value *float64) (bool, error) {
	if err := p.validate(); err != nil {
		return false, err
	}
	return s.recordSample(ctx, dep, slo, p, value)
}

// LastSample returns a series' newest sample.
func (s *Store) LastSample(ctx context.Context, dep sre.UUID, slo,
	series string) (PushSample, bool, error) {
	p := PushSample{Series: series}
	err := s.pool.QueryRow(ctx, `SELECT bad, eligible, observed_at
		FROM sage.sre_sli_samples
		WHERE deployment_id = $1 AND slo_name = $2 AND series = $3
		ORDER BY observed_at DESC LIMIT 1`, string(dep), slo, series).Scan(&p.Bad,
		&p.Eligible, &p.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PushSample{}, false, nil
	}
	if err != nil {
		return PushSample{}, false, fmt.Errorf("read last SLI sample of %s: %w", slo, err)
	}
	return p, true, nil
}

// Aggregate sums a SLO's counter series (or one series) over a window:
// per series the increases over the samples in [from, to] plus the
// newest sample before from (within the lookback) as the baseline; a
// decrease is a reset whose new value is the increase. It reads a few
// samples per series (running counters, store_window.go).
func (s *Store) Aggregate(ctx context.Context, dep sre.UUID, slo, series string, from,
	to time.Time, lookback time.Duration) ([]SeriesAgg, error) {
	out, err := s.aggregate(ctx, dep, slo, series, from, to, lookback)
	if err != nil {
		return nil, fmt.Errorf("aggregate SLI samples of %s: %w", slo, err)
	}
	return out, nil
}

// Baseline is the median of a series' gauge values since a time, and how
// many there were.
func (s *Store) Baseline(ctx context.Context, dep sre.UUID, slo, series string,
	since time.Time) (float64, int, error) {
	var median *float64
	var n int
	err := s.pool.QueryRow(ctx, `SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY value),
		    count(value)::int
		FROM sage.sre_sli_samples
		WHERE deployment_id = $1 AND slo_name = $2 AND series = $3 AND observed_at >= $4
		  AND value IS NOT NULL`, string(dep), slo, series, since).Scan(&median, &n)
	if err != nil {
		return 0, 0, fmt.Errorf("read baseline of %s: %w", slo, err)
	}
	if median == nil {
		return 0, 0, nil
	}
	return *median, n, nil
}
