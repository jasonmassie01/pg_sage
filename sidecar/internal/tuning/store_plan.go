package tuning

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/planfresh"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Plan is the statement's newest fresh cached plan (planfresh: age,
// ANALYZE and pg_sage actions bound it); when every cached plan is stale
// or none exists, one bounded plain EXPLAIN of text. A failing EXPLAIN is
// an error: the stale plan is never returned in its place (tuning-08).
func (s *postgresStore) Plan(ctx context.Context, queryID int64, text string) (Plan, error) {
	r := planfresh.Reader{DB: catalogread.New(s.pool, s.timeouts), MaxAge: s.planMaxAge,
		Explain: s.explainAfresh, ExplainUncached: true}
	return r.Read(ctx, queryID, text)
}

// explainAfresh plans text with EXPLAIN (FORMAT JSON), or GENERIC_PLAN on
// PG16+ for a parameterized statement; before PG16 such a statement has
// no plan (PlanSourceNone).
func (s *postgresStore) explainAfresh(ctx context.Context, text string) (Plan, error) {
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), ";"))
	if body == "" || hasSemicolon(body) {
		return Plan{}, errMultiStatement
	}
	source, explain := PlanSourceExplain, "EXPLAIN (FORMAT JSON) "
	if paramRef.MatchString(workload.BlankLiteralsAndComments(body)) {
		if s.version < 160000 {
			return Plan{Source: PlanSourceNone}, nil
		}
		source, explain = PlanSourceGeneric, "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	plan, err := s.explain(ctx, explain+body)
	if err != nil {
		return Plan{}, fmt.Errorf("explain: %w", err)
	}
	return Plan{Source: source, JSON: plan}, nil
}

// PlanExplainer is the query tuner's fallback when a candidate's cached
// plan is stale: the same bounded plain EXPLAIN the agent uses.
func PlanExplainer(pool *pgxpool.Pool, pgVersion int, t catalogread.Timeouts,
) planfresh.ExplainFunc {
	s := &postgresStore{pool: pool, version: pgVersion, timeouts: t}
	return s.explainAfresh
}
