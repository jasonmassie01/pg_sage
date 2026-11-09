package gameday

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps one deployment's game days in sage.sre_game_days.
type Store struct {
	pool       *pgxpool.Pool
	deployment string
}

var uuidPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewStore binds the game days of deploymentID.
func NewStore(pool *pgxpool.Pool, deploymentID string) (*Store, error) {
	if pool == nil || !uuidPattern.MatchString(deploymentID) {
		return nil, errors.New("game day store needs a pool and a deployment UUID")
	}
	return &Store{pool: pool, deployment: deploymentID}, nil
}

// Run kinds (sage.sre_game_days.kind): a scheduled game day or a local
// bench run. Game-day lists and the schedule read only game days.
const (
	rowGameDay = "game_day"
	rowLocal   = "local"
)

func (s *Store) start(ctx context.Context, gd GameDay, kind string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.sre_game_days
		(deployment_id, id, database_name, provider, families, status, started_at, kind)
		VALUES ($1, $2, $3, $4, $5, 'running', $6, $7)`, s.deployment, gd.ID, gd.Database,
		gd.Provider, gd.Families, gd.StartedAt, kind)
	if err != nil {
		return fmt.Errorf("record %s %s start: %w", kind, gd.ID, err)
	}
	return nil
}

// attachClone records a running run's clone as soon as it exists, so a
// process that stops mid-run leaves a clone the next sweep can destroy.
func (s *Store) attachClone(ctx context.Context, id, cloneID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sage.sre_game_days SET clone_id = $3
		WHERE deployment_id = $1 AND id = $2 AND status = 'running'`, s.deployment, id,
		cloneID)
	if err != nil {
		return fmt.Errorf("record clone %s of run %s: %w", cloneID, id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("record clone %s: run %s is not running", cloneID, id)
	}
	return nil
}

func (s *Store) finish(ctx context.Context, gd GameDay) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sage.sre_game_days
		SET status = $3, finished_at = $4, error = NULLIF($5, ''),
		    clone_id = NULLIF($6, ''), eval_run_id = NULLIF($7, '')::uuid
		WHERE deployment_id = $1 AND id = $2`, s.deployment, gd.ID, gd.Status,
		gd.FinishedAt, gd.Error, gd.CloneID, gd.EvalRunID)
	if err != nil {
		return fmt.Errorf("record game day result: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("game day %s not found", gd.ID)
	}
	return nil
}

const gameDaySelect = `SELECT id::text, database_name, provider, COALESCE(clone_id, ''),
	families, status, started_at, finished_at, COALESCE(error, ''),
	COALESCE(eval_run_id::text, '') FROM sage.sre_game_days`

func scanGameDay(row pgx.Row) (GameDay, error) {
	var gd GameDay
	err := row.Scan(&gd.ID, &gd.Database, &gd.Provider, &gd.CloneID, &gd.Families,
		&gd.Status, &gd.StartedAt, &gd.FinishedAt, &gd.Error, &gd.EvalRunID)
	if gd.Families == nil {
		gd.Families = []string{}
	}
	return gd, err
}

// List lists a database's game days (not its local bench runs), newest first.
func (s *Store) List(ctx context.Context, database string, limit int) ([]GameDay, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, gameDaySelect+` WHERE deployment_id = $1
		AND database_name = $2 AND kind = 'game_day' ORDER BY started_at DESC, id LIMIT $3`,
		s.deployment, database, limit)
	if err != nil {
		return nil, fmt.Errorf("list game days: %w", err)
	}
	defer rows.Close()
	out := []GameDay{}
	for rows.Next() {
		gd, err := scanGameDay(rows)
		if err != nil {
			return nil, fmt.Errorf("scan game day: %w", err)
		}
		out = append(out, gd)
	}
	return out, rows.Err()
}

// Latest is a database's newest game day, or nil.
func (s *Store) Latest(ctx context.Context, database string) (*GameDay, error) {
	gd, err := scanGameDay(s.pool.QueryRow(ctx, gameDaySelect+` WHERE deployment_id = $1
		AND database_name = $2 AND kind = 'game_day' ORDER BY started_at DESC LIMIT 1`,
		s.deployment, database))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest game day: %w", err)
	}
	return &gd, nil
}
