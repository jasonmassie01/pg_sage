package autoexplain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

var observedPlanPrefix = regexp.MustCompile(`^duration:\s+([0-9]+(?:\.[0-9]+)?)\s+ms\s+plan:\s*`)

type ObservedPlan struct {
	QueryID     int64
	Query       string
	JSON        []byte
	TotalCost   float64
	ExecutionMS float64
	CapturedAt  time.Time
}

// ParseObservedPlan accepts PostgreSQL auto_explain JSON objects, never plain duration logs.
func ParseObservedPlan(entry logwatch.LogEntry, database string) (ObservedPlan, error) {
	var result ObservedPlan
	if database == "" || entry.Database != database || entry.Timestamp.IsZero() {
		return result, errors.New("auto_explain log lacks matching database or capture timestamp")
	}
	match := observedPlanPrefix.FindStringSubmatch(entry.Message)
	if match == nil {
		return result, errors.New("log is not a JSON auto_explain plan")
	}
	var doc struct {
		Query   string      `json:"Query Text"`
		QueryID json.Number `json:"Query Identifier"`
		Plan    *struct {
			TotalCost *float64 `json:"Total Cost"`
		} `json:"Plan"`
	}
	body := []byte(entry.Message[len(match[0]):])
	if len(body) > 1<<20 || json.Unmarshal(body, &doc) != nil ||
		strings.TrimSpace(doc.Query) == "" || doc.Plan == nil || doc.Plan.TotalCost == nil ||
		math.IsNaN(*doc.Plan.TotalCost) || math.IsInf(*doc.Plan.TotalCost, 0) || *doc.Plan.TotalCost < 0 {
		return result, errors.New("auto_explain JSON is truncated or lacks query and plan cost")
	}
	queryID, err := parseQueryIdentifier(doc.QueryID)
	if err != nil {
		return result, err
	}
	duration, err := strconv.ParseFloat(match[1], 64)
	if err != nil || math.IsInf(duration, 0) {
		return result, errors.New("invalid plan duration")
	}
	planJSON := append(append([]byte{'['}, body...), ']')
	return ObservedPlan{QueryID: queryID, Query: doc.Query, JSON: planJSON,
		TotalCost: *doc.Plan.TotalCost, ExecutionMS: duration, CapturedAt: entry.Timestamp}, nil
}

func parseQueryIdentifier(number json.Number) (int64, error) {
	if number == "" {
		return 0, nil
	}
	if signed, err := strconv.ParseInt(string(number), 10, 64); err == nil {
		return signed, nil
	}
	if unsigned, err := strconv.ParseUint(string(number), 10, 64); err == nil {
		return int64(unsigned), nil
	}
	return 0, errors.New("invalid auto_explain query identifier")
}

// StoreObservedPlan preserves a real execution's capture time and prevents duplicate delivery.
// Missing query identifiers cannot safely be inferred from literal-bearing SQL and are rejected.
func StoreObservedPlan(ctx context.Context, pool *pgxpool.Pool, plan ObservedPlan) error {
	if pool == nil || plan.QueryID == 0 || plan.CapturedAt.IsZero() || !json.Valid(plan.JSON) {
		return errors.New("observed plan requires a pool, query identifier, timestamp and valid JSON")
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin observed plan store: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// Serialize duplicate deliveries before taking the INSERT statement's fresh snapshot.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("sage.observed_plan:%d", plan.QueryID)); err != nil {
		return fmt.Errorf("lock observed plan delivery: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO sage.explain_cache
		(queryid, query_text, plan_json, source, total_cost, execution_time, captured_at)
		SELECT $1, $2, $3::jsonb, 'auto_explain_log', $4, $5, $6
		WHERE NOT EXISTS (SELECT 1 FROM sage.explain_cache
			WHERE queryid=$1 AND captured_at=$6 AND source='auto_explain_log' AND plan_json=$3::jsonb)`,
		plan.QueryID, plan.Query, plan.JSON, plan.TotalCost, plan.ExecutionMS, plan.CapturedAt)
	if err != nil {
		return fmt.Errorf("store observed auto_explain plan: %w", err)
	}
	return tx.Commit(ctx)
}
