// Package onboarding is pg_sage's first-run experience (roadmap phase 3,
// "five-minute time to value"): it records whether a database is a new
// install or an upgrade, measures the time to the first finding, builds
// the first-run checklist and the guided "grant more" step that shows
// what each trust level allows and which grants it needs.
//
// A new install starts read-only: the default trust level, observation,
// only observes, and nothing changes outside pg_sage's own schema until an
// operator grants more. An existing install keeps its configured trust.
package onboarding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InstallKind tells a new install from an upgrade of an existing one.
type InstallKind string

// Install kinds.
const (
	InstallNew      InstallKind = "new"
	InstallExisting InstallKind = "existing"
)

// Errors, each distinguishable with errors.Is.
var (
	ErrNoPool         = errors.New("onboarding: no database connection pool")
	ErrNoDatabase     = errors.New("onboarding: no database name")
	ErrNotInitialized = errors.New("onboarding: database has no onboarding record")
)

// State is a database's onboarding record.
type State struct {
	Database           string      `json:"database"`
	InstallKind        InstallKind `json:"install_kind"`
	InstalledAt        time.Time   `json:"installed_at"`
	FirstLookAt        *time.Time  `json:"first_look_at,omitempty"`
	FirstFindingAt     *time.Time  `json:"first_finding_at,omitempty"`
	FirstFindingSource string      `json:"first_finding_source,omitempty"`
	TTFFSeconds        *float64    `json:"time_to_first_finding_seconds,omitempty"`
}

func check(pool *pgxpool.Pool, database string) error {
	if pool == nil {
		return ErrNoPool
	}
	if database == "" {
		return ErrNoDatabase
	}
	return nil
}

// initSQL records the database once. pg_sage has run here before when it
// started its trust ramp or collected a snapshot: that install keeps its
// configured trust. Later starts keep the first record. The newest snapshot
// is read through its collected_at index, never by scanning the history.
const initSQL = `INSERT INTO sage.onboarding (database_name, install_kind)
SELECT $1, CASE WHEN EXISTS (SELECT 1 FROM sage.config WHERE key = 'trust_ramp_start')
                  OR EXISTS (SELECT 1 FROM (SELECT collected_at FROM sage.snapshots
                                            ORDER BY collected_at DESC LIMIT 1) newest)
                THEN 'existing' ELSE 'new' END
ON CONFLICT (database_name) DO NOTHING`

// Init records database on its first start under this version and returns
// its state. It must run before the runtime collects or starts its trust
// ramp, which is what marks an install as existing.
func Init(ctx context.Context, pool *pgxpool.Pool, database string) (State, error) {
	if err := check(pool, database); err != nil {
		return State{}, err
	}
	if _, err := pool.Exec(ctx, initSQL, database); err != nil {
		return State{}, fmt.Errorf("record onboarding of %q: %w", database, err)
	}
	st, found, err := Get(ctx, pool, database)
	if err != nil {
		return State{}, err
	}
	if !found {
		return State{}, fmt.Errorf("record onboarding of %q: %w", database, ErrNotInitialized)
	}
	return st, nil
}

// Get returns database's state; found is false before Init.
func Get(ctx context.Context, pool *pgxpool.Pool, database string) (State, bool, error) {
	if err := check(pool, database); err != nil {
		return State{}, false, err
	}
	st := State{Database: database}
	var ttff *int64
	err := pool.QueryRow(ctx, `SELECT install_kind, installed_at, first_look_at,
		first_finding_at, first_finding_source, ttff_ms FROM sage.onboarding
		WHERE database_name = $1`, database).Scan(&st.InstallKind, &st.InstalledAt,
		&st.FirstLookAt, &st.FirstFindingAt, &st.FirstFindingSource, &ttff)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read onboarding of %q: %w", database, err)
	}
	if ttff != nil {
		s := float64(*ttff) / 1000
		st.TTFFSeconds = &s
	}
	return st, true, nil
}

// RecordFirstLook stores when the first first look finished; later ones
// leave it unchanged.
func RecordFirstLook(ctx context.Context, pool *pgxpool.Pool, database string,
	at time.Time) error {
	_, err := recordOnce(ctx, pool, database, `UPDATE sage.onboarding
		SET first_look_at = $2 WHERE database_name = $1 AND first_look_at IS NULL
		RETURNING 1`, at)
	return err
}

// RecordFirstFinding stores the first finding's time, source (first_look
// or analyzer) and time to first finding. It reports false when one was
// already recorded.
func RecordFirstFinding(ctx context.Context, pool *pgxpool.Pool, database string,
	at time.Time, ttff time.Duration, source string) (bool, error) {
	return recordOnce(ctx, pool, database, `UPDATE sage.onboarding
		SET first_finding_at = $2, ttff_ms = $3, first_finding_source = $4
		WHERE database_name = $1 AND first_finding_at IS NULL RETURNING 1`, at,
		max(ttff.Milliseconds(), 0), source)
}

// recordOnce runs a guarded update of database's row: it reports whether
// it changed the row, and ErrNotInitialized when there is no row.
func recordOnce(ctx context.Context, pool *pgxpool.Pool, database, update string,
	args ...any) (bool, error) {
	if err := check(pool, database); err != nil {
		return false, err
	}
	var changed, exists bool
	err := pool.QueryRow(ctx, `WITH upd AS (`+update+`)
		SELECT EXISTS (SELECT 1 FROM upd),
		       EXISTS (SELECT 1 FROM sage.onboarding WHERE database_name = $1)`,
		append([]any{database}, args...)...).Scan(&changed, &exists)
	if err != nil {
		return false, fmt.Errorf("update onboarding of %q: %w", database, err)
	}
	if !exists {
		return false, fmt.Errorf("update onboarding of %q: %w", database, ErrNotInitialized)
	}
	return changed, nil
}

// HasOpenFinding reports whether the analyzer has an open finding.
func HasOpenFinding(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	if pool == nil {
		return false, ErrNoPool
	}
	var open bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sage.findings
		WHERE status = 'open')`).Scan(&open); err != nil {
		return false, fmt.Errorf("read open findings: %w", err)
	}
	return open, nil
}
