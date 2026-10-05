package tuning

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// postgresStore is the agent's Store on PostgreSQL. Every read is one bounded
// statement (the catalog and plan reads in a READ ONLY transaction with
// the safety timeouts); none loops over tables.
type postgresStore struct {
	pool     *pgxpool.Pool
	version  int
	timeouts catalogread.Timeouts
}

// NewPostgresStore returns the Store reading pool; pgVersion decides
// whether a parameterized statement can be planned (GENERIC_PLAN, PG16+).
func NewPostgresStore(pool *pgxpool.Pool, pgVersion int, t catalogread.Timeouts) Store {
	return &postgresStore{pool: pool, version: pgVersion, timeouts: t}
}

const openFindingsSQL = `/* pg_sage */
SELECT category, severity, COALESCE(object_type, ''), COALESCE(object_identifier, ''),
       title, detail, COALESCE(recommendation, ''), COALESCE(recommended_sql, ''),
       COALESCE(rollback_sql, '')
FROM sage.findings
WHERE status = 'open' AND category = ANY($1)
ORDER BY id`

func (s *postgresStore) OpenFindings(ctx context.Context, categories []string) (
	[]analyzer.Finding, error) {
	rows, err := s.pool.Query(ctx, openFindingsSQL, categories)
	if err != nil {
		return nil, fmt.Errorf("read open findings: %w", err)
	}
	defer rows.Close()
	var out []analyzer.Finding
	for rows.Next() {
		var f analyzer.Finding
		if err := rows.Scan(&f.Category, &f.Severity, &f.ObjectType, &f.ObjectIdentifier,
			&f.Title, &f.Detail, &f.Recommendation, &f.RecommendedSQL,
			&f.RollbackSQL); err != nil {
			return nil, fmt.Errorf("scan open finding: %w", err)
		}
		f.ActionRisk, _ = f.Detail["action_risk"].(string)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open findings: %w", err)
	}
	return out, nil
}

const operatorRejectedSQL = `/* pg_sage */
SELECT proposed_sql FROM sage.action_queue
WHERE status = 'rejected' AND decided_by IS NOT NULL AND proposed_at >= $1`

func (s *postgresStore) OperatorRejected(ctx context.Context, since time.Time) (map[string]bool,
	error) {
	rows, err := s.pool.Query(ctx, operatorRejectedSQL, since)
	if err != nil {
		return nil, fmt.Errorf("read operator rejections: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sql string
		if err := rows.Scan(&sql); err != nil {
			return nil, fmt.Errorf("scan operator rejection: %w", err)
		}
		out[normalizeSQL(sql)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read operator rejections: %w", err)
	}
	return out, nil
}

const outcomesSQL = `/* pg_sage */
SELECT action_class, prediction_method,
       COALESCE((predicted->>'expected_change_pct')::float8, 0),
       verdict, tolerance, (observed->>'change_pct')::float8
FROM (SELECT o.*, row_number() OVER (PARTITION BY action_class
                                     ORDER BY decided_at DESC) AS rn
      FROM sage.action_outcome o
      WHERE action_class = ANY($1) AND decided_at IS NOT NULL AND decided_at >= $2
        AND verdict <> 'pending') d
WHERE rn <= $3
ORDER BY decided_at DESC`

func (s *postgresStore) Outcomes(ctx context.Context, classes []string, since time.Time,
	limit int) ([]OutcomeSample, error) {
	if len(classes) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, outcomesSQL, classes, since, limit)
	if err != nil {
		return nil, fmt.Errorf("read outcomes: %w", err)
	}
	defer rows.Close()
	var out []OutcomeSample
	for rows.Next() {
		var o OutcomeSample
		if err := rows.Scan(&o.Class, &o.Method, &o.PredictedPct, &o.Verdict, &o.Tolerance,
			&o.ObservedPct); err != nil {
			return nil, fmt.Errorf("scan outcome: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read outcomes: %w", err)
	}
	return out, nil
}

const cachedPlanSQL = `/* pg_sage */
SELECT plan_json::text FROM sage.explain_cache
WHERE queryid = $1 ORDER BY captured_at DESC LIMIT 1`

// paramRef finds a bind parameter ($1) outside literals and comments.
var paramRef = regexp.MustCompile(`\$[0-9]+`)

// errMultiStatement refuses text that is not one statement.
var errMultiStatement = errors.New("tuning: a plan is read for one statement only")

func (s *postgresStore) Plan(ctx context.Context, queryID int64, text string) (Plan, error) {
	var cached string
	err := s.pool.QueryRow(ctx, cachedPlanSQL, queryID).Scan(&cached)
	switch {
	case err == nil:
		return Plan{Source: PlanSourceCache, JSON: []byte(cached)}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return Plan{}, fmt.Errorf("read explain cache for queryid %d: %w", queryID, err)
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ";"))
	if body == "" || hasSemicolon(body) {
		return Plan{}, errMultiStatement
	}
	source, explain := PlanSourceExplain, "EXPLAIN (FORMAT JSON) "
	if paramRef.MatchString(blankLiteralsAndComments(body)) {
		if s.version < 160000 {
			return Plan{Source: PlanSourceNone}, nil
		}
		source, explain = PlanSourceGeneric, "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	plan, err := s.explain(ctx, explain+body)
	if err != nil {
		return Plan{}, fmt.Errorf("plan queryid %d: %w", queryID, err)
	}
	return Plan{Source: source, JSON: plan}, nil
}

// explain runs one EXPLAIN (never ANALYZE) in a bounded READ ONLY
// transaction that is always rolled back.
func (s *postgresStore) explain(ctx context.Context, sql string) ([]byte, error) {
	tx, err := catalogread.Begin(ctx, s.pool, s.timeouts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	return rawSingleValue(ctx, tx.Conn().PgConn(), sql)
}

// rawSingleValue runs one statement as a simple query, so a generic
// plan's $n reach the server unbound (the caller refused anything but one
// statement), and returns its single value.
func rawSingleValue(ctx context.Context, pc *pgconn.PgConn, sql string) ([]byte, error) {
	results, err := pc.Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(results) != 1 || len(results[0].Rows) == 0 || len(results[0].Rows[0]) == 0 {
		return nil, errors.New("the statement returned no plan")
	}
	return results[0].Rows[0][0], nil
}
