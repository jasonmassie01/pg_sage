package drill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Invariant statuses.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusUnknown = "unknown"
)

// Invariant is one check's result on one table ("" for a whole-database
// check), with the evidence that decided it.
type Invariant struct {
	Name     string `json:"name"`
	Table    string `json:"table,omitempty"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

// Result is one drill, as sage.restore_drills stores it.
type Result struct {
	ID            int64       `json:"id"`
	DatabaseID    int         `json:"database_id,omitempty"`
	Database      string      `json:"database"`
	Provider      string      `json:"provider"`
	Kind          clone.Kind  `json:"kind"`
	ReceiptID     int64       `json:"receipt_id,omitempty"`
	RequestedAt   time.Time   `json:"requested_at"`
	ReadyAt       *time.Time  `json:"ready_at,omitempty"`
	FinishedAt    time.Time   `json:"finished_at"`
	RecoveryPoint time.Time   `json:"recovery_point,omitempty"`
	RTOSeconds    *float64    `json:"rto_seconds,omitempty"`
	RPOSeconds    *float64    `json:"rpo_seconds,omitempty"`
	RPOReason     string      `json:"rpo_reason,omitempty"`
	Invariants    []Invariant `json:"invariants"`
	Passed        bool        `json:"passed"`
	Failure       string      `json:"failure,omitempty"`
	CostLabel     string      `json:"cost_label,omitempty"`
}

// allPassed reports whether no invariant failed; unknown is not a failure.
func allPassed(invs []Invariant) bool {
	for _, inv := range invs {
		if inv.Status == StatusFail {
			return false
		}
	}
	return true
}

// failedSummary names the failed invariants, "" when none failed.
func failedSummary(invs []Invariant) string {
	var failed []string
	for _, inv := range invs {
		if inv.Status == StatusFail {
			failed = append(failed, strings.TrimSpace(inv.Name+" "+inv.Table)+": "+
				inv.Evidence)
		}
	}
	return strings.Join(failed, "; ")
}

// Store keeps drill results in the control database.
type Store struct{ pool *pgxpool.Pool }

// NewStore is the store on pool; a nil pool refuses every call (ErrNoStore).
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) usable() error {
	if s == nil || s.pool == nil {
		return ErrNoStore
	}
	return nil
}

const recordSQL = `/* pg_sage restore_drill v1 */
INSERT INTO sage.restore_drills (database_id, database_name, provider, kind, receipt_id,
    requested_at, ready_at, finished_at, recovery_point, rto_seconds, rpo_seconds,
    invariants, passed, failure, cost_label)
VALUES (NULLIF($1::integer, 0), $2, $3, $4, NULLIF($5::bigint, 0), $6, $7, $8, $9, $10,
    $11, $12::jsonb, $13, NULLIF($14, ''), NULLIF($15, ''))
RETURNING id`

// Record writes one drill row and returns its id.
func (s *Store) Record(ctx context.Context, r Result) (int64, error) {
	if err := s.usable(); err != nil {
		return 0, fmt.Errorf("record restore drill for %s: %w", r.Database, err)
	}
	invs := r.Invariants
	if invs == nil {
		invs = []Invariant{}
	}
	raw, err := json.Marshal(invs)
	if err != nil {
		return 0, fmt.Errorf("record restore drill for %s: encode invariants: %w",
			r.Database, err)
	}
	var rp *time.Time
	if !r.RecoveryPoint.IsZero() {
		rp = &r.RecoveryPoint
	}
	kind := string(r.Kind)
	if kind == "" {
		kind = "unknown"
	}
	var id int64
	err = s.pool.QueryRow(ctx, recordSQL, r.DatabaseID, r.Database, r.Provider, kind,
		r.ReceiptID, r.RequestedAt, r.ReadyAt, r.FinishedAt, rp, r.RTOSeconds, r.RPOSeconds,
		string(raw), r.Passed, r.Failure, r.CostLabel).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record restore drill for %s: %w", r.Database, err)
	}
	return id, nil
}

// LastPassed is when database's newest passed drill finished; zero when
// none passed. A failed drill after a pass does not reset the pass.
func (s *Store) LastPassed(ctx context.Context, database string) (time.Time, error) {
	if err := s.usable(); err != nil {
		return time.Time{}, err
	}
	var at *time.Time
	err := s.pool.QueryRow(ctx, `/* pg_sage restore_drill v1 */
		SELECT max(finished_at) FROM sage.restore_drills
		WHERE database_name = $1 AND passed`, database).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the last passed restore drill of %s: %w",
			database, err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return at.UTC(), nil
}

// Attempt is a database's newest drill, passed or not.
type Attempt struct {
	RequestedAt time.Time
	Passed      bool
}

// LastAttempt is database's newest drill; false when it was never drilled.
func (s *Store) LastAttempt(ctx context.Context, database string) (Attempt, bool, error) {
	if err := s.usable(); err != nil {
		return Attempt{}, false, err
	}
	var a Attempt
	err := s.pool.QueryRow(ctx, `/* pg_sage restore_drill v1 */
		SELECT requested_at, passed FROM sage.restore_drills WHERE database_name = $1
		ORDER BY requested_at DESC, id DESC LIMIT 1`, database).Scan(&a.RequestedAt, &a.Passed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, false, nil
	}
	if err != nil {
		return Attempt{}, false, fmt.Errorf("read the last restore drill of %s: %w",
			database, err)
	}
	a.RequestedAt = a.RequestedAt.UTC()
	return a, true, nil
}
