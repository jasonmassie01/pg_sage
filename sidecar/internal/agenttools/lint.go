package agenttools

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/migration"
)

// Lint bounds and verdict bars.
const (
	maxLintBytes = 100000
	riskyScore   = 0.7
	reviewScore  = 0.3
)

// LintRequest is migration SQL to lint; PGVersion 0 means the server's.
type LintRequest struct {
	SQL       string `json:"sql"`
	PGVersion int    `json:"pg_version,omitempty"`
}

// LintResult is every recognized hazard and the overall verdict.
type LintResult struct {
	Statements   []LintItem `json:"statements"`
	MaxRiskScore float64    `json:"max_risk_score"`
	Verdict      string     `json:"verdict"` // safe | review | risky
	Note         string     `json:"note,omitempty"`
}

// LintItem is one rule a statement matched, scored on live statistics.
type LintItem struct {
	Statement       string  `json:"statement"`
	RuleID          string  `json:"rule_id"`
	LockLevel       string  `json:"lock_level,omitempty"`
	RequiresRewrite bool    `json:"requires_rewrite"`
	Table           string  `json:"table,omitempty"`
	TableSizeBytes  int64   `json:"table_size_bytes"`
	EstimatedRows   int64   `json:"estimated_rows"`
	RiskScore       float64 `json:"risk_score"`
	EstimatedLockMs int64   `json:"estimated_lock_ms"`
	SafeAlternative string  `json:"safe_alternative,omitempty"`
	Description     string  `json:"description,omitempty"`
}

// LintMigration classifies migration SQL against the DDL safety rules and
// scores each hazard with the target table's size and activity. The SQL
// is text to analyze: it is never run.
func (t *Tools) LintMigration(ctx context.Context, req LintRequest) (LintResult, error) {
	if err := t.ready(); err != nil {
		return LintResult{}, err
	}
	if strings.TrimSpace(req.SQL) == "" {
		return LintResult{}, invalid("sql is required")
	}
	if len(req.SQL) > maxLintBytes {
		return LintResult{}, invalid("sql is longer than %d bytes", maxLintBytes)
	}
	if req.PGVersion < 0 {
		return LintResult{}, invalid("pg_version %d is negative", req.PGVersion)
	}
	version, err := t.lintVersion(ctx, req.PGVersion)
	if err != nil {
		return LintResult{}, err
	}
	assessor := migration.NewRiskAssessor(t.pool, t.opts.Log)
	res := LintResult{Statements: []LintItem{}}
	classified := migration.NewRegexClassifier().Classify(req.SQL, version)
	for _, c := range classified {
		risk, err := assessor.Assess(ctx, c)
		if err != nil {
			return LintResult{}, fmt.Errorf("assess %s: %w", c.RuleID, err)
		}
		res.Statements = append(res.Statements, lintItem(risk))
	}
	res.MaxRiskScore, res.Verdict = lintVerdict(res.Statements)
	if len(res.Statements) == 0 {
		res.Note = "No DDL safety rule recognized these statements: nothing is known to " +
			"lock or rewrite a table. Unrecognized DDL is not proven safe."
	}
	return reviewUnproven(res, classified, req.SQL), nil
}

// reviewUnproven raises a safe verdict to review when a statement is
// outside the additive allowlist and no DDL safety rule recognized it, or
// when the text cannot be tokenized (migration-04a): safe must mean the
// statements are known. ddl_missing_lock_timeout is advice about the
// batch, not a recognition of the statement's hazard. It never lowers a
// verdict or changes the score.
func reviewUnproven(
	res LintResult, classified []migration.DDLClassification, sql string,
) LintResult {
	unproven, err := migration.UnprovenStatements(sql)
	if err != nil { // migration.ErrUntokenizable
		return raiseToReview(res, "The SQL could not be tokenized: it ends inside a "+
			"literal, quoted identifier, dollar-quoted body or comment, which can hide "+
			"statements. Review it.")
	}
	recognized := make(map[string]bool, len(classified))
	for _, c := range classified {
		if c.RuleID != "ddl_missing_lock_timeout" {
			recognized[c.Statement] = true
		}
	}
	n := 0
	for _, stmt := range unproven {
		if !recognized[stmt] {
			n++
		}
	}
	switch n {
	case 0:
		return res
	case 1:
		return raiseToReview(res, "1 statement is outside the additive allowlist and "+
			"matches no DDL safety rule: unrecognized DDL is not proven safe. Review it.")
	}
	return raiseToReview(res, fmt.Sprintf("%d statements are outside the additive "+
		"allowlist and match no DDL safety rule: unrecognized DDL is not proven safe. "+
		"Review them.", n))
}

func raiseToReview(res LintResult, note string) LintResult {
	if res.Verdict == "safe" {
		res.Verdict = "review"
	}
	if res.Note != "" {
		note = res.Note + " " + note
	}
	res.Note = note
	return res
}

func (t *Tools) lintVersion(ctx context.Context, requested int) (int, error) {
	if requested > 0 {
		return requested, nil
	}
	var version int
	err := t.pool.QueryRow(ctx,
		"/* pg_sage */ SELECT current_setting('server_version_num')::int").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read server version: %w", err)
	}
	return version, nil
}

func lintItem(r *migration.DDLRisk) LintItem {
	table := r.TableName
	if r.SchemaName != "" && table != "" {
		table = r.SchemaName + "." + table
	}
	return LintItem{Statement: r.Statement, RuleID: r.RuleID, LockLevel: r.LockLevel,
		RequiresRewrite: r.RequiresRewrite, Table: table, TableSizeBytes: r.TableSizeBytes,
		EstimatedRows: r.EstimatedRows, RiskScore: r.RiskScore,
		EstimatedLockMs: r.EstimatedLockMs, SafeAlternative: r.SafeAlternative,
		Description: r.Description}
}

// lintVerdict is risky at riskyScore; review at reviewScore or for any
// rewrite or ACCESS EXCLUSIVE lock (they block the table however small it
// is now); safe otherwise.
func lintVerdict(items []LintItem) (float64, string) {
	maxScore, heavy := 0.0, false
	for _, it := range items {
		maxScore = max(maxScore, it.RiskScore)
		heavy = heavy || it.RequiresRewrite || it.LockLevel == "ACCESS EXCLUSIVE"
	}
	switch {
	case maxScore >= riskyScore:
		return maxScore, "risky"
	case maxScore >= reviewScore || heavy:
		return maxScore, "review"
	}
	return maxScore, "safe"
}
