package drill

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Recoverability (drill-01d) is what one database's drill history says
// about restoring it, and the fleet's count of databases with a passed
// drill in the last 35 days (the spec's number). Each database is in
// exactly one status, so the counts add up to the total.
const (
	// RecoverPassed: the newest passed drill finished at most 35 days ago.
	RecoverPassed = "passed"
	// RecoverFailed: no pass in the window and the newest drill failed.
	RecoverFailed = "failed"
	// RecoverStale: the newest drill passed, but more than 35 days ago.
	RecoverStale = "stale"
	// RecoverNeverDrilled: a provider can drill it, but no drill has run.
	RecoverNeverDrilled = "never_drilled"
	// RecoverUnknown: no drill can run (no provider, or no kind allowed),
	// or its history could not be read.
	RecoverUnknown = "unknown"
)

// RecoverabilityWindow is how old a pass may be and still count; a pass
// exactly 35 days old counts.
const RecoverabilityWindow = 35 * 24 * time.Hour

// History limits for one database's drill list.
const (
	DefaultHistoryLimit = 20
	MaxHistoryLimit     = 100
)

// UnknownAdvice says what to configure and grant when recoverability is
// unknown; recoverability_unknown's recommendation says the same.
const UnknownAdvice = "pg_sage cannot prove this database restores without a clone. " +
	"Set clone.provider (lakebase_branch, neon_branch, aurora_clone or dle run " +
	"drills by default; a full copy needs clone.allow_full_copy and full_copy in " +
	"the policy document's drill.kinds) and grant it what the provider's section " +
	"of docs/managed-clouds.md lists. Until then D7 keeps agent writes at L2."

const neverDrilledReason = "no restore drill has run yet: drills run hourly once the " +
	"policy document's drill block turns them on and one falls due"

// Recoverability is one database's status with the drills that decided it.
type Recoverability struct {
	Database string  `json:"database"`
	Status   string  `json:"status"`
	Reason   string  `json:"reason,omitempty"`
	Advice   string  `json:"advice,omitempty"`
	Latest   *Result `json:"latest,omitempty"`
	LastPass *Result `json:"last_pass,omitempty"`
}

// FleetRecoverability is the count over a set of databases.
type FleetRecoverability struct {
	WindowDays       int              `json:"window_days"`
	Total            int              `json:"total"`
	Passed           int              `json:"passed"`
	Failed           int              `json:"failed"`
	Stale            int              `json:"stale"`
	NeverDrilled     int              `json:"never_drilled"`
	Unknown          int              `json:"unknown"`
	MedianRTOSeconds *float64         `json:"median_rto_seconds"`
	Databases        []Recoverability `json:"databases"`
	Errors           []string         `json:"errors,omitempty"`
}

// ValidHistoryLimit accepts 1..MaxHistoryLimit.
func ValidHistoryLimit(n int) error {
	if n < 1 || n > MaxHistoryLimit {
		return fmt.Errorf("limit must be an integer from 1 to %d", MaxHistoryLimit)
	}
	return nil
}

// Classify is database's status at now from its newest drill (latest),
// its newest passed drill (lastPass) and, when no drill can run, why
// (unknownReason). Recorded drills win over the reason: a pass stays a
// pass after the provider is removed. A failed drill after a pass in the
// window does not reset the pass, as D7 reads it.
func Classify(database string, latest, lastPass *Result, unknownReason string,
	now time.Time) Recoverability {
	r := Recoverability{Database: database, Latest: latest, LastPass: lastPass}
	switch {
	case lastPass != nil && !now.IsZero() &&
		!lastPass.FinishedAt.Before(now.Add(-RecoverabilityWindow)):
		r.Status = RecoverPassed
	case latest != nil && !latest.Passed:
		r.Status, r.Reason = RecoverFailed, latest.Failure
		if r.Reason == "" {
			r.Reason = "the newest restore drill failed"
		}
	case latest != nil:
		r.Status = RecoverStale
		r.Reason = "the newest passed restore drill is more than 35 days old"
	case unknownReason != "":
		r.Status, r.Reason, r.Advice = RecoverUnknown, unknownReason, UnknownAdvice
	default:
		r.Status, r.Reason = RecoverNeverDrilled, neverDrilledReason
	}
	return r
}

// Summarize counts list by status; an unrecognised status counts as
// unknown, never as passed. The median RTO is over the passed databases'
// newest passes that measured one; nil when none did.
func Summarize(list []Recoverability) FleetRecoverability {
	out := FleetRecoverability{WindowDays: int(RecoverabilityWindow / (24 * time.Hour)),
		Total: len(list), Databases: append([]Recoverability{}, list...)}
	var rtos []float64
	for _, r := range list {
		switch r.Status {
		case RecoverPassed:
			out.Passed++
			if r.LastPass != nil && r.LastPass.RTOSeconds != nil {
				rtos = append(rtos, *r.LastPass.RTOSeconds)
			}
		case RecoverFailed:
			out.Failed++
		case RecoverStale:
			out.Stale++
		case RecoverNeverDrilled:
			out.NeverDrilled++
		default:
			out.Unknown++
		}
	}
	out.MedianRTOSeconds = median(rtos)
	return out
}

func median(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	sort.Float64s(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = (v[len(v)/2-1] + m) / 2
	}
	return &m
}

const historyColumns = `id, coalesce(database_id, 0), database_name, provider, kind,
    coalesce(receipt_id, 0), requested_at, ready_at, finished_at, recovery_point,
    rto_seconds, rpo_seconds, invariants, passed, coalesce(failure, ''),
    coalesce(cost_label, '')`

// History is database's newest drills, newest first, at most limit
// (1..MaxHistoryLimit); an empty, non-nil list when it was never drilled.
func (s *Store) History(ctx context.Context, database string, limit int) ([]Result, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	if err := ValidHistoryLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage restore_drill v1 */
		SELECT `+historyColumns+` FROM sage.restore_drills WHERE database_name = $1
		ORDER BY requested_at DESC, id DESC LIMIT $2`, database, limit)
	if err != nil {
		return nil, fmt.Errorf("read the restore drills of %s: %w", database, err)
	}
	out, err := pgx.CollectRows(rows, scanResult)
	if err != nil {
		return nil, fmt.Errorf("read the restore drills of %s: %w", database, err)
	}
	if out == nil {
		out = []Result{}
	}
	return out, nil
}

// LastPassedResult is database's newest passed drill; nil when none passed.
func (s *Store) LastPassedResult(ctx context.Context, database string) (*Result, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage restore_drill v1 */
		SELECT `+historyColumns+` FROM sage.restore_drills
		WHERE database_name = $1 AND passed
		ORDER BY finished_at DESC, id DESC LIMIT 1`, database)
	if err == nil {
		var r Result
		r, err = pgx.CollectExactlyOneRow(rows, scanResult)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err == nil {
			return &r, nil
		}
	}
	return nil, fmt.Errorf("read the last passed restore drill of %s: %w", database, err)
}

func scanResult(row pgx.CollectableRow) (Result, error) {
	var r Result
	var kind string
	var point *time.Time
	err := row.Scan(&r.ID, &r.DatabaseID, &r.Database, &r.Provider, &kind, &r.ReceiptID,
		&r.RequestedAt, &r.ReadyAt, &r.FinishedAt, &point, &r.RTOSeconds, &r.RPOSeconds,
		&r.Invariants, &r.Passed, &r.Failure, &r.CostLabel)
	if err != nil {
		return Result{}, err
	}
	r.Kind = clone.Kind(kind)
	r.RequestedAt, r.FinishedAt = r.RequestedAt.UTC(), r.FinishedAt.UTC()
	if r.ReadyAt != nil {
		ready := r.ReadyAt.UTC()
		r.ReadyAt = &ready
	}
	if point != nil {
		r.RecoveryPoint = point.UTC()
	}
	if r.Invariants == nil {
		r.Invariants = []Invariant{}
	}
	if r.RPOSeconds == nil {
		r.RPOReason = markerEvidence(r.Invariants)
	}
	return r, nil
}

// markerEvidence is why RPO is unknown: the marker invariant's evidence.
func markerEvidence(invs []Invariant) string {
	for _, inv := range invs {
		if inv.Name == "marker" && inv.Status != StatusPass {
			return inv.Evidence
		}
	}
	return ""
}

// UnknownReason is the reason of database's open recoverability_unknown
// finding on pool (the database's own sage schema); "" when none is open.
func UnknownReason(ctx context.Context, pool *pgxpool.Pool, database string) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("read the recoverability of %s: %w", database, ErrNoStore)
	}
	var reason string
	err := pool.QueryRow(ctx, `/* pg_sage restore_drill v1 */
		SELECT coalesce(detail->>'reason', '') FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'
		ORDER BY id DESC LIMIT 1`, CategoryUnknown, objectID(database)).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the recoverability of %s: %w", database, err)
	}
	return reason, nil
}
