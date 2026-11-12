package migration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/catalogread"
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
	llmClient  *llm.Client      // nil = deterministic-only, no LLM fallback
	scriptGen  *ScriptGenerator // nil = deterministic SafeAlternative only
	// enricher runs the model work off the poll path, and cache keeps one
	// answer per statement identity for both detectors (migration-10).
	// Both are nil without a model.
	enricher *Enricher
	cache    *replyCache
}

// WithCatalogReadTimeouts bounds the risk assessment's live-metric reads
// with the safety configuration's timeouts.
func (a *Advisor) WithCatalogReadTimeouts(t catalogread.Timeouts) *Advisor {
	a.assessor.SetCatalogReadTimeouts(t)
	if a.scriptGen != nil {
		a.scriptGen.timeouts = t
	}
	return a
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
		classifier: NewClassifier(),
		assessor:   NewRiskAssessor(pool, logFn),
		pool:       pool,
		cfg:        cfg,
		pgVersion:  pgVersion,
		dbName:     dbName,
		logFn:      logFn,
		llmClient:  llmClient,
	}
	if cfg.DDLRowThreshold > 0 {
		a.assessor.rowThreshold = int64(cfg.DDLRowThreshold)
	}
	if llmClient != nil {
		a.scriptGen = NewScriptGenerator(
			llmClient, pool, pgVersion, logFn)
		a.cache = newReplyCache(enrichCacheSize, enrichCacheTTL)
		a.scriptGen.cache = a.cache
		a.enricher = newEnricher(dbName, enrichQueueSize, a.runEnrichJob, logFn)
	}
	return a
}

// Analyze classifies the SQL, assesses risk for each classification,
// and returns an rca.Incident for the highest-risk finding above the
// threshold (risk_score > 0.3). Returns nil if all risks are low.
//
// The statement is sanitized first (comments stripped, literals
// redacted) so no observed literal reaches a log or the findings table.
// Analyze never calls the model: the detectors queue that work after
// persisting the deterministic finding (queueEnrichment, migration-10).
func (a *Advisor) Analyze(
	ctx context.Context, sql string,
) (*rca.Incident, error) {
	inc, _, err := a.AnalyzeSessionRisk(ctx, sql, "", 0)
	return inc, err
}

// AnalyzeRisk is Analyze that also returns the winning assessment, whose
// live measurements the finding detail carries (migration-01).
func (a *Advisor) AnalyzeRisk(
	ctx context.Context, sql string,
) (*rca.Incident, *DDLRisk, error) {
	return a.AnalyzeSessionRisk(ctx, sql, "", 0)
}

// AnalyzeSession is Analyze for DDL run by role, whose search_path
// resolves unqualified table names ("" when the role is unknown).
func (a *Advisor) AnalyzeSession(
	ctx context.Context, sql, role string,
) (*rca.Incident, error) {
	inc, _, err := a.AnalyzeSessionRisk(ctx, sql, role, 0)
	return inc, err
}

// AnalyzeSessionRisk is AnalyzeSession that also returns the winning
// assessment, for the DDL running in backend pid (0 when unknown), which
// the activity read leaves out. The risk is nil whenever the incident is.
func (a *Advisor) AnalyzeSessionRisk(
	ctx context.Context, sql, role string, pid int,
) (*rca.Incident, *DDLRisk, error) {
	clean := sanitizeDDL(sql)
	classifications := a.classifier.Classify(clean, a.pgVersion)
	if len(classifications) == 0 {
		return nil, nil, nil
	}

	for i := range classifications {
		classifications[i].SessionRole = role
		classifications[i].SessionPID = pid
	}
	highest, lockTimeout := a.assessHighest(ctx, classifications)
	if highest == nil || highest.RiskScore <= incidentThreshold {
		return nil, nil, nil
	}
	incident := a.buildIncident(highest)
	if lockTimeout != nil && highest.LockLevel == "ACCESS EXCLUSIVE" {
		incident.CausalChain = append(incident.CausalChain, rca.ChainLink{
			Order:  len(incident.CausalChain) + 1,
			Signal: lockTimeout.RuleID,
			Description: lockTimeout.Description + ". " +
				lockTimeout.SafeAlternative,
		})
	}
	return incident, highest, nil
}

// assessHighest scores every classification with Review's path
// (scoreClassifications: the lock_timeout annotation is not scored,
// G7-B33) and returns the highest-risk one plus the annotation, if
// present.
func (a *Advisor) assessHighest(
	ctx context.Context, classifications []DDLClassification,
) (*DDLRisk, *DDLClassification) {
	refine := func(c *DDLClassification) { a.refineAlterType(ctx, c) }
	risks, lockTimeout, err := scoreClassifications(ctx, a.assessor, classifications, refine)
	if err != nil {
		a.logFn("warn", "migration: %v", err)
	}
	var highest *DDLRisk
	for _, risk := range risks {
		if highest == nil || risk.RiskScore > highest.RiskScore {
			highest = risk
		}
	}
	return highest, lockTimeout
}

// buildIncident converts a DDLRisk into an rca.Incident whose
// recommendation is the rule's deterministic alternative; the model's
// script, if any, arrives later in the finding's llm_ keys (migration-10).
func (a *Advisor) buildIncident(risk *DDLRisk) *rca.Incident {
	severity := "warning"
	if risk.RiskScore > 0.7 {
		severity = "critical"
	}

	tableFQN := qualifiedName(risk.SchemaName, risk.TableName)
	chain := buildCausalChain(risk)

	return &rca.Incident{
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
		// The rule's own template: the only text a finding recommends.
		RecommendedSQLSource: SQLSourceDeterministic,
	}
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
	if risk.NameAmbiguous {
		chain = append(chain, rca.ChainLink{
			Order:  len(chain) + 1,
			Signal: "name_resolution",
			Description: fmt.Sprintf("table name %q is ambiguous: it matches "+
				"several schemas, so size and lock statistics are unknown",
				risk.TableName),
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
