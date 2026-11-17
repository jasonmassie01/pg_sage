package modelpass

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// RunRecord is one row of sage.review_model_run: a model run written once
// with its final status. OutputSHA256 hashes the whole raw output, which
// is stored capped at MaxRawOutputBytes (Truncated says it was cut).
// CostUSD is nil when the model is unpriced.
type RunRecord struct {
	RunID         int64                    `json:"run_id"`
	Attempt       int                      `json:"attempt"`
	Role          string                   `json:"role"`
	Model         string                   `json:"model"`
	PromptVersion string                   `json:"prompt_version"`
	PackHash      string                   `json:"pack_hash"`
	RawOutput     string                   `json:"raw_output"`
	Truncated     bool                     `json:"raw_truncated"`
	OutputSHA256  string                   `json:"output_sha256"`
	KeptClaims    int                      `json:"kept_claims"`
	DroppedClaims []agentloop.DroppedClaim `json:"dropped_claims"`
	TokensIn      int64                    `json:"tokens_in"`
	TokensOut     int64                    `json:"tokens_out"`
	CostUSD       *float64                 `json:"cost_usd"`
	Status        string                   `json:"status"`
	ErrorClass    string                   `json:"error_class,omitempty"`
	Reason        string                   `json:"reason,omitempty"`
}

// Store writes model runs to sage.review_model_run. It only inserts: the
// table refuses UPDATEs.
type Store struct{ pool *pgxpool.Pool }

// NewStore is a store over the sage schema's pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const insertRunSQL = `/* pg_sage review v1 */
INSERT INTO sage.review_model_run (run_id, attempt, role, model, prompt_version, pack_hash,
    raw_output, raw_truncated, output_sha256, kept_claims, dropped_claims, tokens_in,
    tokens_out, cost_usd, status, error_class, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`

// RecordRun inserts r.
func (s *Store) RecordRun(ctx context.Context, r RunRecord) error {
	_, err := s.pool.Exec(ctx, insertRunSQL, r.RunID, r.Attempt, r.Role, r.Model,
		r.PromptVersion, r.PackHash, r.RawOutput, r.Truncated, r.OutputSHA256, r.KeptClaims,
		recordJSON(r.DroppedClaims), r.TokensIn, r.TokensOut, r.CostUSD, r.Status,
		r.ErrorClass, clip(r.Reason, 1000))
	if err != nil {
		return fmt.Errorf("insert model run %d of review run %d: %w", r.Attempt, r.RunID, err)
	}
	return nil
}

const listRunsSQL = `/* pg_sage review v1 */
SELECT run_id, attempt, role, model, prompt_version, pack_hash, raw_output, raw_truncated,
       output_sha256, kept_claims, dropped_claims, tokens_in, tokens_out,
       cost_usd::float8, status, error_class, reason
  FROM sage.review_model_run WHERE run_id = $1 ORDER BY attempt`

// ListRuns returns a review run's model runs by attempt.
func (s *Store) ListRuns(ctx context.Context, runID int64) ([]RunRecord, error) {
	rows, err := s.pool.Query(ctx, listRunsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list model runs of review run %d: %w", runID, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RunRecord, error) {
		var r RunRecord
		var dropped []byte
		err := row.Scan(&r.RunID, &r.Attempt, &r.Role, &r.Model, &r.PromptVersion,
			&r.PackHash, &r.RawOutput, &r.Truncated, &r.OutputSHA256, &r.KeptClaims,
			&dropped, &r.TokensIn, &r.TokensOut, &r.CostUSD, &r.Status, &r.ErrorClass,
			&r.Reason)
		if err == nil {
			err = json.Unmarshal(dropped, &r.DroppedClaims)
		}
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read model runs of review run %d: %w", runID, err)
	}
	return out, nil
}
