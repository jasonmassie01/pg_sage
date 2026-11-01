package autoexplain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

const LakebasePlanSource = "lakebase_plan_history"

// ProviderPlan carries a captured workload plan, not a request to run EXPLAIN.
// AnalyzeEnabled is independent of capture: a logged plan may contain estimates.
type ProviderPlan struct {
	ObservedPlan
	ScopeHash      string
	ComputeID      string
	EventID        string
	ReceivedAt     time.Time
	AnalyzeEnabled *bool
	BuffersEnabled *bool
	TimingEnabled  *bool
	Fingerprint    string
	DurationSource string
	ComparisonKey  string
}

// StoreProviderPlan is idempotent at the monitored sink, including a crash after
// this commit but before the control inbox acknowledges delivery.
func StoreProviderPlan(ctx context.Context, pool *pgxpool.Pool, p ProviderPlan) error {
	if pool == nil || p.QueryID == 0 || p.ScopeHash == "" || p.ComputeID == "" || p.EventID == "" ||
		p.CapturedAt.IsZero() || p.ReceivedAt.IsZero() || len(p.JSON) == 0 || len(p.JSON) > 1<<20 ||
		!json.Valid(p.JSON) || math.IsNaN(p.TotalCost) || math.IsInf(p.TotalCost, 0) || p.TotalCost < 0 ||
		math.IsNaN(p.ExecutionMS) || math.IsInf(p.ExecutionMS, 0) || p.ExecutionMS < 0 {
		return errors.New("provider plan requires valid resource, occurrence, timestamps and bounded plan data")
	}
	if selfmonitor.IsQueryText(p.Query) {
		return ErrSelfStatement
	}
	hash := planHashParam(p.JSON)
	if hash == nil {
		return errors.New("provider plan has no valid EXPLAIN shape")
	}
	_, err := pool.Exec(ctx, `/* pg_sage */ INSERT INTO sage.explain_cache
 (queryid,query_text,plan_json,source,total_cost,execution_time,captured_at,plan_hash,
 provider_scope_hash,provider_compute_id,provider_event_id,received_at,comparison_key,
 analyze_enabled,buffers_enabled,timing_enabled,provider_plan_fingerprint,duration_source)
 VALUES ($1,$2,$3::jsonb,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
 ON CONFLICT (provider_scope_hash,provider_event_id) WHERE provider_event_id IS NOT NULL DO NOTHING`,
		p.QueryID, p.Query, p.JSON, LakebasePlanSource, p.TotalCost, p.ExecutionMS, p.CapturedAt, hash,
		p.ScopeHash, p.ComputeID, p.EventID, p.ReceivedAt, nullableComparisonKey(p.ComparisonKey), p.AnalyzeEnabled, p.BuffersEnabled, p.TimingEnabled, p.Fingerprint, p.DurationSource)
	if err != nil {
		return fmt.Errorf("store captured provider plan: %w", err)
	}
	return nil
}

func nullableComparisonKey(key string) *string {
	if key == "" {
		return nil
	}
	return &key
}
