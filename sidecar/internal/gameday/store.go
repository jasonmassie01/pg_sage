package gameday

import (
	"context"
	"encoding/json"
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
	exercised, err := encodeExercised(gd)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO sage.sre_game_days
		(deployment_id, id, database_name, provider, families, status, started_at, kind,
		 exercised)
		VALUES ($1, $2, $3, $4, $5, 'running', $6, $7, $8::jsonb)`, s.deployment, gd.ID,
		gd.Database, gd.Provider, gd.Families, gd.StartedAt, kind, exercised)
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
	summary := []byte("{}")
	if gd.Summary != nil {
		var err error
		if summary, err = json.Marshal(gd.Summary); err != nil {
			return fmt.Errorf("encode game day %s summary: %w", gd.ID, err)
		}
	}
	exercised, err := encodeExercised(gd)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sage.sre_game_days
		SET status = $3, finished_at = $4, error = NULLIF($5, ''),
		    clone_id = NULLIF($6, ''), eval_run_id = NULLIF($7, '')::uuid,
		    summary = $8::jsonb, exercised = COALESCE($9::jsonb, exercised)
		WHERE deployment_id = $1 AND id = $2`, s.deployment, gd.ID, gd.Status,
		gd.FinishedAt, gd.Error, gd.CloneID, gd.EvalRunID, string(summary), exercised)
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
	COALESCE(eval_run_id::text, ''), summary::text, COALESCE(exercised::text, '')
	FROM sage.sre_game_days`

func scanGameDay(row pgx.Row) (GameDay, error) {
	var gd GameDay
	var summary, exercised string
	err := row.Scan(&gd.ID, &gd.Database, &gd.Provider, &gd.CloneID, &gd.Families,
		&gd.Status, &gd.StartedAt, &gd.FinishedAt, &gd.Error, &gd.EvalRunID, &summary,
		&exercised)
	if gd.Families == nil {
		gd.Families = []string{}
	}
	if err != nil {
		return gd, err
	}
	gd.Summary, err = decodeSummary(summary)
	if err == nil {
		gd.Exercised, err = decodeExercised(exercised)
	}
	if err != nil {
		err = fmt.Errorf("game day %s: %w", gd.ID, err)
	}
	return gd, err
}

// decodeSummary reads a stored summary; '{}' (a row written before
// benches-08, or a run without a report) is no summary.
func decodeSummary(raw string) (*Summary, error) {
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var s Summary
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("decode summary: %w", err)
	}
	if s.TopSkips == nil {
		s.TopSkips = []SkipCount{}
	}
	return &s, nil
}

// encodeExercised is gd's exercised record as JSON, or nil (SQL NULL)
// when it has none.
func encodeExercised(gd GameDay) (any, error) {
	if gd.Exercised == nil {
		return nil, nil
	}
	raw, err := json.Marshal(gd.Exercised)
	if err != nil {
		return nil, fmt.Errorf("encode game day %s exercised: %w", gd.ID, err)
	}
	return string(raw), nil
}

// decodeExercised reads a stored record; NULL (a row before benches-12, a
// local bench run) is none.
func decodeExercised(raw string) (*Exercised, error) {
	if raw == "" {
		return nil, nil
	}
	var x Exercised
	if err := json.Unmarshal([]byte(raw), &x); err != nil {
		return nil, fmt.Errorf("decode exercised: %w", err)
	}
	if x.Scenarios == nil {
		x.Scenarios = []ExercisedScenario{}
	}
	return &x, nil
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
