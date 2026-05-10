package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

func TestOptimizerRecommendationToFinding_SeparatesActionLevelFromRisk(t *testing.T) {
	rec := optimizer.Recommendation{
		Table:                   "public.orders",
		DDL:                     "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)",
		DropDDL:                 "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_status",
		Rationale:               "speed up status lookups",
		Severity:                "warning",
		Confidence:              0.62,
		IndexType:               "btree",
		Category:                "missing_index",
		EstimatedImprovementPct: 25,
		ActionLevel:             "advisory",
		AffectedQueries:         []string{"SELECT * FROM orders WHERE status = $1"},
	}
	result := &optimizer.Result{PlanSource: "pg_stat_statements"}

	finding := optimizerRecommendationToFinding(rec, result)

	if finding.ActionRisk != optimizer.RiskModerate {
		t.Fatalf("ActionRisk = %q, want %q",
			finding.ActionRisk, optimizer.RiskModerate)
	}
	if finding.ActionRisk == rec.ActionLevel {
		t.Fatalf("ActionRisk reused ActionLevel %q", rec.ActionLevel)
	}
	if finding.Detail["action_level"] != rec.ActionLevel {
		t.Fatalf("detail action_level = %v, want %q",
			finding.Detail["action_level"], rec.ActionLevel)
	}
	if finding.RecommendedSQL != rec.DDL {
		t.Fatalf("RecommendedSQL = %q, want %q",
			finding.RecommendedSQL, rec.DDL)
	}
	if finding.RollbackSQL != rec.DropDDL {
		t.Fatalf("RollbackSQL = %q, want %q",
			finding.RollbackSQL, rec.DropDDL)
	}
}

func TestOptimizerSpecializedRecommendationsProjectToIndexActions(t *testing.T) {
	tests := []struct {
		name      string
		category  string
		indexType string
		ddl       string
		query     string
	}{
		{
			name:      "json",
			category:  "json_index_recommendation",
			indexType: "gin",
			ddl: "CREATE INDEX CONCURRENTLY idx_events_payload_gin " +
				"ON public.events USING gin (payload jsonb_path_ops);",
			query: "SELECT * FROM events WHERE payload @> '{\"tier\":\"pro\"}'",
		},
		{
			name:      "vector",
			category:  "vector_index_recommendation",
			indexType: "hnsw",
			ddl: "CREATE INDEX CONCURRENTLY idx_docs_embedding_hnsw " +
				"ON public.docs USING hnsw (embedding vector_cosine_ops);",
			query: "SELECT id FROM docs ORDER BY embedding <-> $1 LIMIT 10",
		},
		{
			name:      "postgis",
			category:  "postgis_index_recommendation",
			indexType: "gist",
			ddl: "CREATE INDEX CONCURRENTLY idx_places_geom_gist " +
				"ON public.places USING gist (geom);",
			query: "SELECT * FROM places WHERE ST_DWithin(geom, $1, 1000)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finding := optimizerRecommendationToFinding(
				optimizer.Recommendation{
					Table:           "public." + tt.name + "_demo",
					DDL:             tt.ddl,
					DropDDL:         "DROP INDEX CONCURRENTLY IF EXISTS idx_demo;",
					Rationale:       "specialized workload index",
					Severity:        "warning",
					Confidence:      0.72,
					IndexType:       tt.indexType,
					Category:        tt.category,
					ActionLevel:     "advisory",
					AffectedQueries: []string{tt.query},
				},
				&optimizer.Result{PlanSource: "test"},
			)

			projected := cases.ProjectFinding(cases.SourceFinding{
				ID:               "finding-" + tt.name,
				DatabaseName:     "prod",
				Category:         finding.Category,
				Severity:         cases.Severity(finding.Severity),
				ObjectType:       finding.ObjectType,
				ObjectIdentifier: finding.ObjectIdentifier,
				Title:            finding.Title,
				Recommendation:   finding.Recommendation,
				RecommendedSQL:   finding.RecommendedSQL,
				RollbackSQL:      finding.RollbackSQL,
				Detail:           finding.Detail,
			})

			if len(projected.ActionCandidates) != 1 {
				t.Fatalf("ActionCandidates = %d, want 1",
					len(projected.ActionCandidates))
			}
			action := projected.ActionCandidates[0]
			if action.ActionType != "create_index_concurrently" {
				t.Fatalf("ActionType = %q", action.ActionType)
			}
			if action.ScriptOutput == nil {
				t.Fatal("expected migration script output")
			}
			if action.ScriptOutput.MigrationSQL != tt.ddl {
				t.Fatalf("MigrationSQL = %q, want %q",
					action.ScriptOutput.MigrationSQL, tt.ddl)
			}
			if projected.Evidence[0].Detail["affected_queries"] == nil {
				t.Fatal("expected affected query evidence")
			}
		})
	}
}
