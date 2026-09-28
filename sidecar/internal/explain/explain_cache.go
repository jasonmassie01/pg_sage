package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ---------- cache ----------

func (ex *Explainer) checkCache(
	ctx context.Context, hash int64, dbName string,
) (*ExplainResult, error) {
	const q = `SELECT plan_json, explanation, created_at
		FROM sage.explain_results
		WHERE query_hash = $1 AND database_name = $2
		  AND expires_at > now()
		ORDER BY created_at DESC LIMIT 1`

	row := ex.pool.QueryRow(ctx, q, hash, dbName)

	var planRaw, explanationRaw []byte
	var createdAt time.Time
	if err := row.Scan(&planRaw, &explanationRaw, &createdAt); err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("cache query: %w", err)
	}

	var result ExplainResult
	if err := json.Unmarshal(explanationRaw, &result); err != nil {
		return nil, fmt.Errorf("cache unmarshal: %w", err)
	}
	result.PlanJSON = json.RawMessage(planRaw)
	result.CachedAt = &createdAt
	return &result, nil
}

func (ex *Explainer) saveCache(
	ctx context.Context, hash int64, dbName string, result *ExplainResult,
) error {
	ttl := ex.cfg.CacheTTLMinutes
	if ttl == 0 {
		ttl = 60
	}

	explanationJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal explanation: %w", err)
	}

	const q = `INSERT INTO sage.explain_results
		(query_hash, expires_at, plan_json, explanation, database_name)
		VALUES ($1, now() + $2 * interval '1 minute', $3, $4, $5)
		ON CONFLICT (query_hash, database_name) DO UPDATE
		SET plan_json = EXCLUDED.plan_json,
		    explanation = EXCLUDED.explanation,
		    expires_at = EXCLUDED.expires_at,
		    created_at = now()`

	_, err = ex.pool.Exec(
		ctx, q, hash, ttl, result.PlanJSON, explanationJSON, dbName,
	)
	if err != nil {
		return fmt.Errorf("cache insert: %w", err)
	}
	return nil
}
