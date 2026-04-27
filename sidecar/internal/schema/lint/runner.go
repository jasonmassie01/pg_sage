package lint

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// schemaLintCategoryPrefix is the sage.findings.category prefix that
// marks a row as produced by the schema-lint subsystem. The full
// category is "schema_lint:" + rule_id (e.g. "schema_lint:missing_pk").
//
// Queries that want "all lint findings regardless of rule" should use
// category LIKE 'schema_lint:%'.
const schemaLintCategoryPrefix = "schema_lint:"

// Runner orchestrates periodic lint scans and persists results to
// sage.findings (unified with Tier-1 analyzer findings since v0.11).
type Runner struct {
	linter       *Linter
	pool         *pgxpool.Pool
	cfg          *config.SchemaLintConfig
	databaseName string
	logFn        func(string, string, ...any)
}

// NewRunner creates a lint runner.
func NewRunner(
	pool *pgxpool.Pool,
	cfg *config.SchemaLintConfig,
	pgVer int,
	databaseName string,
	logFn func(string, string, ...any),
) *Runner {
	return &Runner{
		linter:       New(pool, cfg, pgVer, logFn),
		pool:         pool,
		cfg:          cfg,
		databaseName: databaseName,
		logFn:        logFn,
	}
}

// SetLLMClient sets the optional LLM client for enhanced analysis.
func (r *Runner) SetLLMClient(c *llm.Client) { r.linter.SetLLMClient(c) }

// Run starts the lint scan loop. Blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	interval := time.Duration(r.cfg.ScanIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	r.logFn("INFO", "schema lint runner started, interval=%s",
		interval)

	// Run once immediately.
	r.scan(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.logFn("INFO", "schema lint runner stopped")
			return
		case <-ticker.C:
			r.scan(ctx)
		}
	}
}

func (r *Runner) scan(ctx context.Context) {
	start := time.Now()
	findings, err := r.linter.Scan(ctx)
	if err != nil {
		r.logFn("ERROR", "schema lint scan failed: %v", err)
		return
	}
	elapsed := time.Since(start)

	if err := r.upsertFindings(ctx, findings); err != nil {
		r.logFn("ERROR", "schema lint persist: %v", err)
		return
	}
	if err := r.resolveCleared(ctx, findings); err != nil {
		r.logFn("WARN", "schema lint resolve cleared: %v", err)
	}

	r.logFn("INFO", "schema lint: %d findings in %s",
		len(findings), elapsed.Truncate(time.Millisecond))
}

// objectIdentifier builds the unified sage.findings.object_identifier
// for a lint Finding. The format keeps schema-qualified names readable
// while still being a unique dedup key.
//
//	"<schema>.<table>"
//	"<schema>.<table>/column=<col>"
//	"<schema>.<table>/index=<idx>"
//	"<schema>.<table>/column=<col>/index=<idx>"
func objectIdentifier(f Finding) string {
	var b strings.Builder
	b.WriteString(f.Schema)
	b.WriteByte('.')
	b.WriteString(f.Table)
	if f.Column != "" {
		b.WriteString("/column=")
		b.WriteString(f.Column)
	}
	if f.Index != "" {
		b.WriteString("/index=")
		b.WriteString(f.Index)
	}
	return b.String()
}

// objectType returns the finding target kind for sage.findings.object_type.
func objectType(f Finding) string {
	switch {
	case f.Index != "":
		return "index"
	case f.Column != "":
		return "column"
	default:
		return "table"
	}
}

// toAnalyzerFinding projects a lint.Finding into the generic
// analyzer.Finding shape expected by analyzer.UpsertFindings.
func (r *Runner) toAnalyzerFinding(f Finding) analyzer.Finding {
	detail := map[string]any{
		"thematic_category": f.Category,
		"schema_name":       f.Schema,
		"table_name":        f.Table,
		"column_name":       f.Column,
		"index_name":        f.Index,
		"impact":            f.Impact,
		"table_size":        f.TableSize,
		"query_count":       f.QueryCount,
		"database_name":     r.databaseName,
	}
	return analyzer.Finding{
		Category:         schemaLintCategoryPrefix + f.RuleID,
		Severity:         f.Severity,
		ObjectType:       objectType(f),
		ObjectIdentifier: objectIdentifier(f),
		Title:            f.Description,
		Detail:           detail,
		Recommendation:   f.Suggestion,
		RecommendedSQL:   f.SQL,
		RuleID:           f.RuleID,
		DatabaseName:     r.databaseName,
	}
}

func (r *Runner) upsertFindings(
	ctx context.Context, findings []Finding,
) error {
	if len(findings) == 0 {
		return nil
	}
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		out = append(out, r.toAnalyzerFinding(f))
	}
	if err := analyzer.UpsertFindings(ctx, r.pool, out); err != nil {
		return fmt.Errorf("upsert lint findings: %w", err)
	}
	return nil
}

// resolveCleared marks previously-open lint findings as resolved when
// they no longer appear in the current scan. Because sage.findings
// dedups by (category, object_identifier), one pass per distinct
// rule_id is required: if we scanned across the whole 'schema_lint:%'
// bucket we'd have to rebuild the per-rule active set anyway.
func (r *Runner) resolveCleared(
	ctx context.Context, findings []Finding,
) error {
	// Collect every rule_id that appeared in this scan so we can
	// resolve its unseen members. Also gather rule_ids that have
	// open findings in the DB but produced zero results now — those
	// also need resolution with an empty active set.
	activeByRule := make(map[string]map[string]bool)
	for _, f := range findings {
		if _, ok := activeByRule[f.RuleID]; !ok {
			activeByRule[f.RuleID] = make(map[string]bool)
		}
		activeByRule[f.RuleID][objectIdentifier(f)] = true
	}

	// Find rule_ids that currently have open lint findings but
	// weren't emitted by this scan. analyzer.UpsertFindings /
	// ResolveCleared operate on a per-pool sage schema, so the
	// database_name in detail is informational only — no filter
	// needed here.
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT rule_id
		 FROM sage.findings
		 WHERE status = 'open'
		   AND category LIKE $1
		   AND rule_id IS NOT NULL`,
		schemaLintCategoryPrefix+"%")
	if err != nil {
		return fmt.Errorf("query open lint rule_ids: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			return fmt.Errorf("scan rule_id: %w", err)
		}
		if _, present := activeByRule[rid]; !present {
			activeByRule[rid] = map[string]bool{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rule_ids: %w", err)
	}

	for ruleID, active := range activeByRule {
		category := schemaLintCategoryPrefix + ruleID
		if err := analyzer.ResolveCleared(
			ctx, r.pool, active, category,
		); err != nil {
			return fmt.Errorf(
				"resolve cleared rule %s: %w", ruleID, err)
		}
	}
	return nil
}
