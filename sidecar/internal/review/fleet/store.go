package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Ledger on the control database: sage.review_fleet_pass and
// sage.review_fleet_member, scoped to one fleet (fleet learning's scope).
type Store struct {
	pool  *pgxpool.Pool
	scope string
}

// NewStore is the ledger of scope on pool.
func NewStore(pool *pgxpool.Pool, scope string) *Store {
	return &Store{pool: pool, scope: scope}
}

var errNoPool = errors.New("review fleet: no control database pool")

func (s *Store) usable() error {
	if s == nil || s.pool == nil {
		return errNoPool
	}
	if s.scope == "" {
		return errors.New("review fleet: no fleet scope")
	}
	return nil
}

// exec runs a state move that must touch want rows; fewer is ErrState.
func (s *Store) exec(ctx context.Context, what string, want int64, sql string,
	args ...any) error {
	if err := s.usable(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if tag.RowsAffected() != want {
		return fmt.Errorf("%w: %s moved %d of %d members", ErrState, what, tag.RowsAffected(),
			want)
	}
	return nil
}

// StartPass opens a pass of the scope in period.
func (s *Store) StartPass(ctx context.Context, period string, members int) (int64, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	var id int64
	err := s.pool.QueryRow(ctx, `/* pg_sage review fleet */
		INSERT INTO sage.review_fleet_pass (fleet_scope, period, members)
		VALUES ($1, $2, $3) RETURNING id`, s.scope, period, members).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert sage.review_fleet_pass for %s: %w", period, err)
	}
	return id, nil
}

// SaveRollups stores every member of pass in state rollup.
func (s *Store) SaveRollups(ctx context.Context, passID int64, rs []Rollup) error {
	if len(rs) == 0 {
		return nil
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("encode the rollups of pass %d: %w", passID, err)
	}
	return s.exec(ctx, fmt.Sprintf("insert the rollups of pass %d", passID), int64(len(rs)),
		`/* pg_sage review fleet */
		INSERT INTO sage.review_fleet_member (pass_id, database_name, rollup)
		SELECT $1, e->>'database', e FROM jsonb_array_elements($2::jsonb) e`, passID, b)
}

// MarkTriaged moves the outliers of pass from rollup to triaged.
func (s *Store) MarkTriaged(ctx context.Context, passID int64, os []Outlier) error {
	if len(os) == 0 {
		return nil
	}
	b, err := json.Marshal(os)
	if err != nil {
		return fmt.Errorf("encode the triage of pass %d: %w", passID, err)
	}
	return s.exec(ctx, fmt.Sprintf("triage pass %d", passID), int64(len(os)),
		`/* pg_sage review fleet */
		UPDATE sage.review_fleet_member m SET state = 'triaged',
		       score = (e->>'score')::float8, reason = e->>'reason', source = e->>'source',
		       cites = ARRAY(SELECT jsonb_array_elements_text(coalesce(e->'cites', '[]')))
		  FROM jsonb_array_elements($2::jsonb) e
		 WHERE m.pass_id = $1 AND m.database_name = e->>'database' AND m.state = 'rollup'`,
		passID, b)
}

// MarkScheduled moves d's member from triaged to deep_scheduled.
func (s *Store) MarkScheduled(ctx context.Context, d Deep, ref int64) error {
	return s.exec(ctx, fmt.Sprintf("schedule the deep review of %s", d.Database), 1,
		`/* pg_sage review fleet */
		UPDATE sage.review_fleet_member SET state = 'deep_scheduled', deep_tokens = $3,
		       trigger_id = $4, scheduled_period = $5, scheduled_at = now(), deferred_to = NULL
		 WHERE pass_id = $1 AND database_name = $2 AND state = 'triaged'`,
		d.PassID, d.Database, d.Tokens, ref, d.Period)
}

// MarkDeferred leaves a triaged member waiting until period to.
func (s *Store) MarkDeferred(ctx context.Context, passID int64, database, to string) error {
	return s.exec(ctx, fmt.Sprintf("defer %s to %s", database, to), 1,
		`/* pg_sage review fleet */
		UPDATE sage.review_fleet_member SET deferred_to = $3
		 WHERE pass_id = $1 AND database_name = $2 AND state = 'triaged'`,
		passID, database, to)
}

// MarkReviewed moves a member from deep_scheduled to reviewed (runID 0:
// the run is not known).
func (s *Store) MarkReviewed(ctx context.Context, passID int64, database string,
	runID int64) error {
	return s.exec(ctx, fmt.Sprintf("mark %s reviewed", database), 1,
		`/* pg_sage review fleet */
		UPDATE sage.review_fleet_member SET state = 'reviewed', reviewed_at = now(),
		       run_id = nullif($3, 0)
		 WHERE pass_id = $1 AND database_name = $2 AND state = 'deep_scheduled'`,
		passID, database, runID)
}

// FinishPass records a running pass's outcome.
func (s *Store) FinishPass(ctx context.Context, passID int64, sum PassSummary) error {
	triage, err1 := json.Marshal(sum.Triage)
	fleet, err2 := json.Marshal(sum.Fleet)
	tiers, err3 := json.Marshal(tierTokens(sum))
	if err := errors.Join(err1, err2, err3); err != nil {
		return fmt.Errorf("encode the outcome of pass %d: %w", passID, err)
	}
	var mode any
	if sum.Triage.Mode != "" {
		mode = sum.Triage.Mode
	}
	return s.exec(ctx, fmt.Sprintf("finish pass %d", passID), 1,
		`/* pg_sage review fleet */
		UPDATE sage.review_fleet_pass SET status = $2, outliers = $3, scheduled = $4,
		       deferred = $5, triage_mode = $6, triage_error_class = $7, triage = $8,
		       fleet_review = $9, tier_tokens = $10, error = $11, finished_at = now()
		 WHERE id = $1 AND status = 'running'`, passID, sum.Status, sum.Outliers,
		sum.Scheduled, sum.Deferred, mode, sum.Triage.ErrorClass, triage, fleet, tiers,
		sum.Error)
}

// tierTokens is the per-tier spend a pass records, keyed by purpose.
func tierTokens(sum PassSummary) map[string]any {
	return map[string]any{
		PurposeTriage: sum.Triage.Usage,
		PurposeFleet:  sum.Fleet.Usage,
		PurposeDeep: map[string]int{"allocated_tokens": sum.DeepTokens,
			"scheduled": sum.Scheduled},
	}
}

// LastPass is the status of the scope's newest finished pass in period.
func (s *Store) LastPass(ctx context.Context, period string) (string, bool, error) {
	if err := s.usable(); err != nil {
		return "", false, err
	}
	var status string
	err := s.pool.QueryRow(ctx, `/* pg_sage review fleet */
		SELECT status FROM sage.review_fleet_pass
		 WHERE fleet_scope = $1 AND period = $2 AND status <> 'running'
		 ORDER BY id DESC LIMIT 1`, s.scope, period).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the last fleet pass of %s: %w", period, err)
	}
	return status, true, nil
}
