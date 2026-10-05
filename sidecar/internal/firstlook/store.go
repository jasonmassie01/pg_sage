package firstlook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// retainReports is how many first looks each database keeps.
const retainReports = 10

// Store keeps first-look reports in sage.first_look of the monitored
// database.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns the store of pool's database.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) ready() error {
	if s == nil || s.pool == nil {
		return ErrNoPool
	}
	return nil
}

// Save records r and keeps only the newest reports of its database.
func (s *Store) Save(ctx context.Context, r Report) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	if r.Database == "" {
		return 0, ErrNoDatabase
	}
	items, checks, caps, err := encodeReport(r)
	if err != nil {
		return 0, err
	}
	var id int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO sage.first_look (database_name, provider,
			started_at, finished_at, duration_ms, relations, statement_timeout_ms, items,
			checks, capabilities) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id`, r.Database, r.Provider, r.StartedAt, r.FinishedAt,
			max(r.DurationMS, 0), r.Relations, r.StatementTimeoutMS, items, checks,
			caps).Scan(&id); err != nil {
			return fmt.Errorf("insert first look: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sage.first_look WHERE database_name = $1
			AND id < (SELECT min(id) FROM (SELECT id FROM sage.first_look
			WHERE database_name = $1 ORDER BY id DESC LIMIT $2) newest)`,
			r.Database, retainReports); err != nil {
			return fmt.Errorf("trim first looks: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("save first look of %q: %w", r.Database, err)
	}
	return id, nil
}

func encodeReport(r Report) ([]byte, []byte, []byte, error) {
	items, err := json.Marshal(nonNil(r.Items))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode first look items: %w", err)
	}
	checks, err := json.Marshal(nonNil(r.Checks))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode first look checks: %w", err)
	}
	caps, err := json.Marshal(nonNil(r.Capabilities))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode first look capabilities: %w", err)
	}
	return items, checks, caps, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// Latest returns the newest report of database; found is false when none
// exists yet.
func (s *Store) Latest(ctx context.Context, database string) (Report, bool, error) {
	if err := s.ready(); err != nil {
		return Report{}, false, err
	}
	var r Report
	var items, checks, caps []byte
	err := s.pool.QueryRow(ctx, `SELECT id, database_name, provider, started_at,
		finished_at, duration_ms, relations, statement_timeout_ms, items, checks,
		capabilities, summary, summary_model FROM sage.first_look
		WHERE database_name = $1 ORDER BY id DESC LIMIT 1`, database).Scan(&r.ID,
		&r.Database, &r.Provider, &r.StartedAt, &r.FinishedAt, &r.DurationMS, &r.Relations,
		&r.StatementTimeoutMS, &items, &checks, &caps, &r.Summary, &r.SummaryModel)
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, fmt.Errorf("read first look of %q: %w", database, err)
	}
	for _, part := range []struct {
		raw []byte
		out any
	}{{items, &r.Items}, {checks, &r.Checks}, {caps, &r.Capabilities}} {
		if err := json.Unmarshal(part.raw, part.out); err != nil {
			return Report{}, false, fmt.Errorf("decode first look of %q: %w", database, err)
		}
	}
	return r, true, nil
}

// SetSummary attaches the model's summary to report id.
func (s *Store) SetSummary(ctx context.Context, id int64, summary, model string) error {
	if err := s.ready(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sage.first_look SET summary = $2,
		summary_model = $3 WHERE id = $1`, id, summary, model)
	if err != nil {
		return fmt.Errorf("save first look summary %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("first look %d: %w", id, ErrNotFound)
	}
	return nil
}
