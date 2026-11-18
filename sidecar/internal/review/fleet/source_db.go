package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/gen/capacity"
	"github.com/pg-sage/sidecar/internal/review/report"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

const (
	// rollupTimeout bounds one member's rollup reads.
	rollupTimeout = 30 * time.Second
	// idleWindowDays is the history the idle rule reads.
	idleWindowDays = 30
	// hoursPerMonth prices compute by the month.
	hoursPerMonth = 730
)

// PoolSource reads a member's rollup from its own sage schema, read-only:
// the newest two review grades, the newest run's candidates, the idle
// rule over its statement history, its size and, with operator rates
// (cost.rates), its monthly cost. Fingerprints maps a member to its
// schema-shape key (SchemaKeys).
type PoolSource struct {
	Pool         func(database string) *pgxpool.Pool
	Fingerprints map[string]string
	Rates        cost.RateSource
	Now          func() time.Time
}

// Rollup implements Source.
func (s *PoolSource) Rollup(ctx context.Context, db string) (Rollup, error) {
	var pool *pgxpool.Pool
	if s != nil && s.Pool != nil {
		pool = s.Pool(db)
	}
	if pool == nil {
		return Rollup{}, fmt.Errorf("no connection pool for member %s", db)
	}
	ctx, cancel := context.WithTimeout(ctx, rollupTimeout)
	defer cancel()
	r := Rollup{Database: db, Fingerprint: s.Fingerprints[db]}
	if err := readGrade(ctx, pool, db, &r); err != nil {
		return r, err
	}
	s.readIdle(ctx, pool, &r)
	size, err := readSize(ctx, pool)
	if err != nil {
		return r, fmt.Errorf("read the size of %s: %w", db, err)
	}
	r.SizeBytes = size
	r.MonthlyUSD = s.monthlyCost(db, size)
	return r, nil
}

// readGrade fills the grade, its predecessor and the newest run's
// candidates.
func readGrade(ctx context.Context, pool *pgxpool.Pool, db string, r *Rollup) error {
	runs, err := rubric.NewStore(pool).Latest(ctx, db, 2)
	if err != nil {
		return fmt.Errorf("read the review grades of %s: %w", db, err)
	}
	if len(runs) == 0 {
		return nil
	}
	g := runs[0].Grade
	r.Graded, r.Grade, r.Score, r.CoveragePct = true, string(g.Overall), g.OverallScore,
		g.CoveragePct
	if len(runs) > 1 {
		prev := runs[1].Grade.OverallScore
		r.PrevScore = &prev
	}
	cands, err := candidate.NewStore(pool).List(ctx, runs[0].ID)
	if err != nil {
		return fmt.Errorf("read the candidates of review run %d of %s: %w", runs[0].ID, db,
			err)
	}
	summarize(cands, r)
	return nil
}

func summarize(cands []candidate.Stored, r *Rollup) {
	if len(cands) == 0 {
		return
	}
	r.Candidates = map[string]int{}
	for _, c := range cands {
		r.Candidates[c.Generator]++
		r.Kinds = append(r.Kinds, c.Generator+"/"+c.Kind)
		r.TopImpact = max(r.TopImpact, report.Score(c.Candidate))
	}
	slices.Sort(r.Kinds)
	r.Kinds = slices.Compact(r.Kinds)
}

// readIdle applies the idle rule (capacity.Idle); unread history leaves
// idleness unknown, never assumed.
func (s *PoolSource) readIdle(ctx context.Context, pool *pgxpool.Pool, r *Rollup) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	days, err := capacity.NewDBCallSource(pool).Read(ctx,
		now.UTC().AddDate(0, 0, -idleWindowDays), now.UTC())
	if err != nil {
		return
	}
	r.IdleKnown, r.QuietDays, r.Idle = true, capacity.QuietDays(days), capacity.Idle(days)
}

// readSize reads the database's size in a bounded read-only transaction.
func readSize(ctx context.Context, pool *pgxpool.Pool) (size int64, err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, ignoreClosed(tx.Rollback(ctx))) }()
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '10s'`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		return 0, err
	}
	err = tx.QueryRow(ctx, `/* pg_sage review fleet */
		SELECT pg_database_size(current_database())`).Scan(&size)
	return size, err
}

func ignoreClosed(err error) error {
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}

// monthlyCost is the member's compute and storage a month at its
// operator rates; nil without rates.
func (s *PoolSource) monthlyCost(db string, size int64) *float64 {
	if s.Rates == nil {
		return nil
	}
	rates, ok := s.Rates.Rates("", db)
	if !ok {
		return nil
	}
	usd := rates.ComputePerHour*hoursPerMonth + rates.StorageGBMonth*float64(size)/(1<<30)
	return &usd
}

// SchemaKeys maps each fingerprinted member to its schema-shape key: the
// hash of its sorted table shapes, so members built from the same schema
// share a key. Members without table shapes have none.
func SchemaKeys(fps []fleetlearn.Fingerprint) map[string]string {
	out := make(map[string]string, len(fps))
	for _, fp := range fps {
		if len(fp.Tables) == 0 {
			continue
		}
		shapes := slices.Clone(fp.Tables)
		slices.Sort(shapes)
		sum := sha256.Sum256([]byte(strings.Join(slices.Compact(shapes), "\n")))
		out[fp.Database] = hex.EncodeToString(sum[:])[:16]
	}
	return out
}
