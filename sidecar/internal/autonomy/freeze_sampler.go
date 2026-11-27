package autonomy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// freezeHorizonLimit bounds the horizons one reading holds for the
// custodian: the most advanced first, so a table beyond it is less urgent
// than every one read.
const freezeHorizonLimit = 500

// ownRelationAllowance is read on top of freezeHorizonLimit for pg_sage's
// own relations: the sage schema's tables and their TOAST (about 380 in
// T6). The custodian leaves them to autovacuum (freezeRows), so without
// the allowance they would crowd user tables out of the reading whenever
// they are the most advanced. TestOwnRelationAllowanceCoversTheSageSchema
// fails when the schema outgrows it.
const ownRelationAllowance = 500

// txBeginner starts the reading's read-only transaction (*pgxpool.Pool).
type txBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// FreezeReading is one sample of a database's freeze state: the
// consumption rates, the xmin holders and the table horizons, all read in
// one read-only transaction (one age() reference).
type FreezeReading struct {
	At       time.Time
	Holders  []freeze.XminBlocker
	Horizons []probes.FreezeHorizon
	rates    freezeRateSample
}

// FreezeSampler is a database's only reader of freeze horizons and its
// only XID and multixact rate state. The freeze custodian, the runway
// advisor and the analyzer share it: a reading younger than its TTL, or
// one taken while a caller waited for it, is served again, so parallel
// readers run one query and see the same rates.
type FreezeSampler struct {
	pool          txBeginner
	bloatMinBytes int64
	// bounds are the reading transaction's statement and lock timeouts.
	bounds catalogread.Timeouts
	ttl    time.Duration
	now    func() time.Time

	mu sync.Mutex
	// readings counts completed readings (read before mu is taken).
	readings     atomic.Int64
	last         freezeCounters
	lastMxidRate float64
	cached       *FreezeReading
}

// NewFreezeSampler reads through pool; minBloatBytes is the bloat plan
// floor (heaps at it get a bloat estimate). A zero TTL serves only
// readings taken while a caller waited. Each reading runs under the
// shipped catalog read bounds (catalogread.Default) until the custodian's
// WithBloatBounds applies the configured ones.
func NewFreezeSampler(pool txBeginner, minBloatBytes int64, ttl time.Duration) *FreezeSampler {
	return &FreezeSampler{pool: pool, bloatMinBytes: minBloatBytes, ttl: max(ttl, 0),
		bounds: catalogread.Default(), now: time.Now}
}

// Read returns the current reading, taking a new one when the cached one
// is older than the TTL and none completed while this caller waited.
func (s *FreezeSampler) Read(ctx context.Context) (FreezeReading, error) {
	if err := ctx.Err(); err != nil {
		return FreezeReading{}, err
	}
	// The counter is read before the clock: a reading that completes after
	// the clock is read is always counted as completed while this caller
	// waited.
	seen := s.readings.Load()
	asked := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// A reading completed since this caller arrived is as fresh as one it
	// would take (the clock may not tell them apart).
	if c := s.cached; c != nil && (s.readings.Load() > seen || asked.Sub(c.At) < s.ttl) {
		return *c, nil
	}
	reading, err := s.read(ctx)
	if err != nil {
		return FreezeReading{}, err
	}
	s.cached = &reading
	s.readings.Add(1)
	return reading, nil
}

// FreezeHorizons is the analyzer's view of the current reading.
func (s *FreezeSampler) FreezeHorizons(ctx context.Context) ([]probes.FreezeHorizon, error) {
	reading, err := s.Read(ctx)
	if err != nil {
		return nil, err
	}
	return reading.Horizons, nil
}

// read samples the counters, the holders and the horizons in one bounded
// read-only transaction (statement and lock timeouts); the caller holds mu.
func (s *FreezeSampler) read(ctx context.Context) (FreezeReading, error) {
	if s.pool == nil {
		return FreezeReading{}, fmt.Errorf("freeze sampler has no database pool")
	}
	tx, err := catalogread.Begin(ctx, s.pool, s.bounds)
	if err != nil {
		return FreezeReading{}, fmt.Errorf("begin freeze age read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // read-only
	cur, failsafeAge, err := readFreezeCounters(ctx, tx)
	if err != nil {
		return FreezeReading{}, freezeReadError(err)
	}
	holders, err := readXminHolders(ctx, tx)
	if err != nil {
		return FreezeReading{}, freezeReadError(err)
	}
	horizons, err := probes.ReadFreezeHorizons(ctx, tx,
		freezeHorizonLimit+ownRelationAllowance, s.bloatMinBytes)
	if err != nil {
		return FreezeReading{}, freezeReadError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return FreezeReading{}, fmt.Errorf("end freeze age read: %w", err)
	}
	cur.at = s.now()
	return FreezeReading{At: cur.at, Holders: holders, Horizons: horizons,
		rates: s.advance(cur, failsafeAge)}, nil
}

// SQLSTATEs of the reading's server-side bounds.
const (
	sqlStateQueryCanceled = "57014" // statement_timeout (or a cancel)
	sqlStateLockTimeout   = "55P03"
)

// freezeReadError names the reading and, when a server-side bound ended
// it, which one: "lock timeout" or "statement timeout" (a slow catalog, or
// a held lock when the statement bound is the shorter, as shipped). The
// driver's error stays wrapped.
func freezeReadError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == sqlStateLockTimeout:
			return fmt.Errorf("freeze age read: lock timeout: %w", err)
		case pgErr.Code == sqlStateQueryCanceled &&
			strings.Contains(pgErr.Message, "statement timeout"):
			return fmt.Errorf("freeze age read: statement timeout: %w", err)
		}
	}
	return fmt.Errorf("freeze age read: %w", err)
}

// advance turns a counter sample into rates against the previous one.
func (s *FreezeSampler) advance(cur freezeCounters, failsafeAge int64) freezeRateSample {
	xid, mxid, known := freezeRates(s.last, cur)
	s.last = cur
	if mxid > 0 {
		s.lastMxidRate = mxid
	} else if s.lastMxidRate > 0 {
		mxid = s.lastMxidRate
	}
	if !known {
		return freezeRateSample{xid: minimumFreezeRate, mxid: minimumFreezeRate,
			failsafeAge: failsafeAge}
	}
	return freezeRateSample{xid: xid, mxid: math.Max(mxid, minimumFreezeRate), known: true,
		failsafeAge: failsafeAge}
}

func readFreezeCounters(ctx context.Context, tx pgx.Tx) (freezeCounters, int64, error) {
	var cur freezeCounters
	var failsafe *string
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT
		pg_snapshot_xmax(pg_current_snapshot())::text::bigint,
		mxid_age(datminmxid)::bigint,
		current_setting('vacuum_failsafe_age', true)
		FROM pg_database WHERE datname=current_database()`).
		Scan(&cur.nextXID, &cur.mxidAge, &failsafe); err != nil {
		return freezeCounters{}, 0, fmt.Errorf("read transaction counters: %w", err)
	}
	return cur, parseFailsafeAge(failsafe), nil
}

// freezeCounters is one sample of the XID and multixact consumption
// counters. nextXID is the snapshot xmax (it advances only for transactions
// that were assigned an XID, i.e. the ones that consume wraparound runway).
type freezeCounters struct {
	at      time.Time
	nextXID int64
	mxidAge int64
}

type freezeRateSample struct {
	xid, mxid float64
	known     bool
	// failsafeAge is vacuum_failsafe_age (PG14+), 0 when the server does
	// not report it; deadline evidence only, read with the counters.
	failsafeAge int64
}

const minimumFreezeRate = 0.001

// freezeRates derives per-second rates from two samples. The first sample
// yields no rate; a negative multixact delta (datminmxid advanced) yields an
// unknown multixact rate (-1).
func freezeRates(prev, cur freezeCounters) (float64, float64, bool) {
	if prev.at.IsZero() || !cur.at.After(prev.at) {
		return 0, 0, false
	}
	seconds := cur.at.Sub(prev.at).Seconds()
	xid := math.Max(float64(cur.nextXID-prev.nextXID)/seconds, minimumFreezeRate)
	mxid := -1.0
	if delta := cur.mxidAge - prev.mxidAge; delta >= 0 {
		mxid = math.Max(float64(delta)/seconds, minimumFreezeRate)
	}
	return xid, mxid, true
}

// parseFailsafeAge reads vacuum_failsafe_age best-effort: a missing
// setting (PG13 and older, or hidden by a managed provider) or an
// unparsable or non-positive value is unknown (0).
func parseFailsafeAge(setting *string) int64 {
	if setting == nil {
		return 0
	}
	age, err := strconv.ParseInt(strings.TrimSpace(*setting), 10, 64)
	if err != nil || age <= 0 {
		return 0
	}
	return age
}
