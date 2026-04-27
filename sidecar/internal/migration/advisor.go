package migration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/rca"
)

// Advisor ties together classification, risk assessment, and incident
// generation for DDL statements. It operates in advisory mode only.
type Advisor struct {
	classifier SQLParser
	assessor   *RiskAssessor
	pool       *pgxpool.Pool
	cfg        *config.MigrationConfig
	pgVersion  int
	dbName     string
	logFn      func(string, string, ...any)
	llmClient  *llm.Client          // nil = deterministic-only, no LLM fallback
	scriptGen  *ScriptGenerator     // nil = deterministic SafeAlternative only
}

// NewAdvisor creates an Advisor. If cfg.Mode is not "advisory", it
// logs a warning and falls back to advisory mode.
func NewAdvisor(
	pool *pgxpool.Pool,
	cfg *config.MigrationConfig,
	pgVersion int,
	dbName string,
	logFn func(string, string, ...any),
	llmClient *llm.Client,
) *Advisor {
	if cfg.Mode != "" && cfg.Mode != "advisory" {
		logFn("warn",
			"migration: mode %q not supported yet, falling back to advisory",
			cfg.Mode)
	}
	a := &Advisor{
		classifier: NewRegexClassifier(),
		assessor:   NewRiskAssessor(pool, logFn),
		pool:       pool,
		cfg:        cfg,
		pgVersion:  pgVersion,
		dbName:     dbName,
		logFn:      logFn,
		llmClient:  llmClient,
	}
	if llmClient != nil {
		a.scriptGen = NewScriptGenerator(
			llmClient, pool, pgVersion, logFn)
	}
	return a
}

// Analyze classifies the SQL, assesses risk for each classification,
// and returns an rca.Incident for the highest-risk finding above the
// threshold (risk_score > 0.3). Returns nil if all risks are low.
func (a *Advisor) Analyze(
	ctx context.Context, sql string,
) (*rca.Incident, error) {
	classifications := a.classifier.Classify(sql, a.pgVersion)
	if len(classifications) == 0 {
		if a.llmClient != nil && a.llmClient.IsEnabled() {
			return a.llmFallback(ctx, sql)
		}
		return nil, nil
	}

	var highest *DDLRisk
	for _, c := range classifications {
		risk, err := a.assessor.Assess(ctx, c)
		if err != nil {
			a.logFn("warn",
				"migration: risk assessment failed for %s: %v",
				c.RuleID, err)
			continue
		}
		if highest == nil || risk.RiskScore > highest.RiskScore {
			highest = risk
		}
	}
	if highest == nil || highest.RiskScore <= 0.3 {
		return nil, nil
	}

	return a.buildIncident(ctx, highest), nil
}

// buildIncident converts a DDLRisk into an rca.Incident. When a
// ScriptGenerator is available, it enhances RecommendedSQL with an
// LLM-generated migration script.
func (a *Advisor) buildIncident(
	ctx context.Context, risk *DDLRisk,
) *rca.Incident {
	severity := "warning"
	if risk.RiskScore > 0.7 {
		severity = "critical"
	}

	tableFQN := qualifiedName(risk.SchemaName, risk.TableName)
	chain := buildCausalChain(risk)

	incident := &rca.Incident{
		DetectedAt:      time.Now(),
		Severity:        severity,
		Source:          "schema_advisor",
		RootCause:       fmt.Sprintf("Dangerous DDL: %s", risk.RuleID),
		CausalChain:     chain,
		AffectedObjects: []string{tableFQN},
		SignalIDs:       []string{risk.RuleID},
		RecommendedSQL:  risk.SafeAlternative,
		ActionRisk:      fmt.Sprintf("risk_score=%.2f", risk.RiskScore),
		Confidence:      risk.RiskScore,
		DatabaseName:    a.dbName,
	}

	if a.scriptGen != nil {
		if script, err := a.scriptGen.Generate(ctx, risk); err == nil && script != "" {
			incident.RecommendedSQL = script
		}
	}

	return incident
}

func buildCausalChain(risk *DDLRisk) []rca.ChainLink {
	chain := []rca.ChainLink{
		{
			Order:       1,
			Signal:      risk.RuleID,
			Description: risk.Description,
			Evidence:    truncateSQL(risk.Statement, 200),
		},
		{
			Order:       2,
			Signal:      "lock_analysis",
			Description: fmt.Sprintf("Requires %s lock", risk.LockLevel),
			Evidence: fmt.Sprintf(
				"table_size=%d rows=%d active_queries=%d",
				risk.TableSizeBytes, risk.EstimatedRows,
				risk.ActiveQueries),
		},
	}
	if risk.SafeAlternative != "" {
		chain = append(chain, rca.ChainLink{
			Order:       3,
			Signal:      "safe_alternative",
			Description: risk.SafeAlternative,
		})
	}
	return chain
}

func qualifiedName(schema, table string) string {
	if schema != "" {
		return schema + "." + table
	}
	if table != "" {
		return "public." + table
	}
	return ""
}

func truncateSQL(sql string, maxLen int) string {
	s := strings.TrimSpace(sql)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
