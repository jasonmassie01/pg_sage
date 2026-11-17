// Package drill runs restore drills (drill-01a): it asks a receipted clone
// provider for a copy of a database at its latest restorable point, proves
// the copy is not the source, checks read-only invariants on it, destroys
// it and records the measured RTO and RPO. A passed drill is what lets D7
// lift its L2 cap on production agent writes.
package drill

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Defaults and bounds of a drill.
const (
	// DefaultMaxDuration bounds a whole drill, the clone's creation
	// included (a full-copy restore takes hours).
	DefaultMaxDuration = 4 * time.Hour
	// DefaultStatementTimeout bounds each invariant statement.
	DefaultStatementTimeout = 30 * time.Second
	// DestroyTimeout bounds the teardown, which runs even when the drill's
	// context is done.
	DestroyTimeout = 10 * time.Minute
	// recordTimeout bounds writing the result row.
	recordTimeout = 30 * time.Second
	// receiptMargin is how long a drill receipt outlives the drill budget.
	receiptMargin = time.Hour
)

// Drill errors.
var (
	// ErrNoProvider: no clone provider is configured, so recoverability is
	// unknown (never a pass).
	ErrNoProvider = errors.New("restore drill: no clone provider: recoverability unknown")
	// ErrNotReceipted: the provider writes no receipts, so a crash could
	// leave a copy of production data behind; nothing is created.
	ErrNotReceipted = errors.New("restore drill: the clone provider writes no receipts")
	// ErrNoStore: no database to record drills in.
	ErrNoStore = errors.New("restore drill: no control database for drill results")
)

// Budget bounds one drill.
type Budget struct {
	MaxDuration      time.Duration
	StatementTimeout time.Duration
}

func (b Budget) resolved() Budget {
	if b.MaxDuration <= 0 {
		b.MaxDuration = DefaultMaxDuration
	}
	if b.StatementTimeout <= 0 {
		b.StatementTimeout = DefaultStatementTimeout
	}
	return b
}

// ReceiptTTL is how long a drill clone's receipt lives: the drill budget
// plus an hour, so the reaper never destroys a clone a drill still uses.
func ReceiptTTL(b Budget) time.Duration { return b.resolved().MaxDuration + receiptMargin }

// Invariants configures the checks beyond the built-in ones.
type Invariants struct {
	// MarkerQuery is the operator's latest-committed-marker query (the
	// policy document's drill.marker_query), run on the clone only; ""
	// when the sage schema is the marker or there is none.
	MarkerQuery string
}

// Drill is one restore drill of one database.
type Drill struct {
	Database   string
	DatabaseID int
	// Source is the monitored database: only the read-only, bounded
	// invariant queries ever run on it.
	Source *pgxpool.Pool
	// Production is the source's server identity for the guard; nil is
	// clone.PoolIdentity(Source).
	Production clone.IdentitySource
	// Provider is a receipted provider (NewReceiptedProvider).
	Provider     clone.Provider
	ProviderName string
	Kind         clone.Kind
	CostLabel    string
	Invariants   Invariants
	Budget       Budget
	Store        *Store
	Logf         func(format string, args ...any)
	Now          func() time.Time
}

func (d Drill) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d Drill) logf(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

func (d Drill) validate() error {
	switch {
	case d.Provider == nil:
		return ErrNoProvider
	case d.Database == "":
		return errors.New("restore drill: database name is required")
	case d.Store == nil:
		return ErrNoStore
	}
	if _, ok := d.Provider.(clone.ReceiptNamer); !ok {
		return ErrNotReceipted
	}
	return nil
}

// Run runs the drill: read the source's catalog, create the clone through
// the receipted provider, connect only through the identity guard, check
// the invariants read-only, destroy the clone (always) and record the
// result. Its error is a drill that could not complete (the failed row is
// still recorded when the store allows); a completed drill whose
// invariants failed returns a result with Passed false and no error.
func (d Drill) Run(ctx context.Context) (Result, error) {
	if err := d.validate(); err != nil {
		return Result{Database: d.Database, Failure: err.Error()}, err
	}
	d.Budget = d.Budget.resolved()
	res := Result{Database: d.Database, DatabaseID: d.DatabaseID, Provider: d.ProviderName,
		Kind: d.Kind, CostLabel: d.CostLabel, RequestedAt: d.now()}
	if res.Provider == "" {
		res.Provider = "unnamed"
	}
	d.logf("restore drill %s: requesting a %s clone from %s (%s)", d.Database, d.Kind,
		res.Provider, d.CostLabel)
	runCtx, cancel := context.WithTimeout(ctx, d.Budget.MaxDuration)
	stepErr := d.restoreAndCheck(runCtx, &res)
	cancel()
	res.FinishedAt = d.now()
	if stepErr != nil {
		res.Passed, res.Failure = false, stepErr.Error()
	} else {
		res.Passed = allPassed(res.Invariants)
		res.Failure = failedSummary(res.Invariants)
	}
	recCtx, recCancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer recCancel()
	id, err := d.Store.Record(recCtx, res)
	if err != nil {
		return res, errors.Join(stepErr, err)
	}
	res.ID = id
	return res, stepErr
}

// restoreAndCheck creates the clone, checks it and destroys it. The
// destroy is deferred, so it runs whatever the checks do.
func (d Drill) restoreAndCheck(ctx context.Context, res *Result) error {
	src, err := readSource(ctx, d.Source, d.Budget.StatementTimeout)
	if err != nil {
		return err
	}
	cl, err := d.Provider.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		return fmt.Errorf("create drill clone: %s: %w", res.Provider, err)
	}
	if namer, ok := d.Provider.(clone.ReceiptNamer); ok {
		res.ReceiptID, _ = namer.ReceiptOf(cl.ID)
	}
	defer d.destroy(ctx, cl, res.ReceiptID)
	ready := d.now()
	res.ReadyAt = &ready
	res.RecoveryPoint = cl.CreatedFrom.UTC()
	if cl.CreatedFrom.IsZero() {
		res.RecoveryPoint = res.RequestedAt
	}
	return d.checkClone(ctx, cl, src, res)
}

// checkClone connects through the guard and runs the invariants.
func (d Drill) checkClone(ctx context.Context, cl clone.Clone, src sourceFacts,
	res *Result) error {
	production := d.Production
	if production == nil {
		production = clone.PoolIdentity(d.Source)
	}
	pool, err := clone.GuardedConnect(ctx, cl, production)
	if errors.Is(err, clone.ErrTargetIsProduction) {
		return fmt.Errorf("identity guard: target_is_production: %w", err)
	}
	if err != nil {
		return fmt.Errorf("connect drill clone %s: %w", cl.ID, err)
	}
	defer pool.Close()
	facts, err := readClone(ctx, pool, src, d.Invariants, res.RecoveryPoint,
		d.Budget.StatementTimeout)
	if err != nil {
		return err
	}
	exact, err := readSourceExact(ctx, d.Source, src, facts, res.RecoveryPoint,
		d.Budget.StatementTimeout)
	if err != nil {
		return err
	}
	res.Invariants = evaluate(src, facts, exact)
	done := d.now()
	rto := math.Max(0, done.Sub(res.RequestedAt).Seconds())
	res.RTOSeconds = &rto
	if rpo, ok := rpoSeconds(res.RecoveryPoint, facts.marker); ok {
		res.RPOSeconds = &rpo
	} else {
		res.RPOReason = facts.markerReason
	}
	return nil
}

// destroy tears the clone down on a context of its own: the drill's may
// be done. A failure leaves the receipt destroying for the reaper.
func (d Drill) destroy(ctx context.Context, cl clone.Clone, receipt int64) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DestroyTimeout)
	defer cancel()
	if err := d.Provider.Destroy(dctx, cl); err != nil {
		d.logf("restore drill %s: clone %s (receipt %d) not destroyed, left for the "+
			"clone reaper: %v", d.Database, cl.ID, receipt, err)
	}
}

// rpoSeconds is the recovery point minus the newest marker on the clone;
// false when no marker was found. A marker after the recovery point (clock
// skew) is 0.
func rpoSeconds(recoveryPoint, marker time.Time) (float64, bool) {
	if marker.IsZero() || recoveryPoint.IsZero() {
		return 0, false
	}
	return math.Max(0, recoveryPoint.Sub(marker).Seconds()), true
}
