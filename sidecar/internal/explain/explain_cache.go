package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llmFallbackTTLMinutes caches a result whose LLM enhancement failed only
// briefly, so a transient provider error does not pin the deterministic
// fallback for the whole TTL.
const llmFallbackTTLMinutes = 1

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

// cacheTTLMinutes is the configured TTL (default when <= 0), cut to
// llmFallbackTTLMinutes when the LLM was configured but failed.
func (ex *Explainer) cacheTTLMinutes(llmDegraded bool) int {
	ttl := config.DefaultExplainCacheTTLMinutes
	if ex.cfg != nil && ex.cfg.CacheTTLMinutes > 0 {
		ttl = ex.cfg.CacheTTLMinutes
	}
	if llmDegraded && ttl > llmFallbackTTLMinutes {
		return llmFallbackTTLMinutes
	}
	return ttl
}

func (ex *Explainer) saveCache(
	ctx context.Context, hash int64, dbName string, result *ExplainResult, ttl int,
) error {

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

// cacheKey is the explain_results key: the query hash mixed with the mode,
// so a plan-only result never answers an ANALYZE request or vice versa.
func cacheKey(query string, params []string, planOnly bool) int64 {
	h := fnv.New64a()
	mode := "analyze"
	if planOnly {
		mode = "plan_only"
	}
	_, _ = h.Write([]byte(mode))
	_, _ = h.Write([]byte{0})
	_, _ = fmt.Fprint(h, queryHash(query, params))
	return int64(h.Sum64())
}
