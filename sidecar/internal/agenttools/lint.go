package agenttools

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/migration"
)

// Lint bounds and verdict bars.
const (
	maxLintBytes = 100000
	riskyScore   = 0.7
	reviewScore  = 0.3
)

// LintRequest is migration SQL to lint. PGVersion is a major (14-18) or a
// server_version_num (140000-189999); 0 means the server's.
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
// is text to analyze: it is never run. It classifies with the same entry
// point as the advisor and the agent gate (migration.ClassifyStatements),
// including the binary-coercible ALTER COLUMN TYPE check, so lint and the
// advisor agree on a rewrite.
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
	version, err := t.lintVersion(ctx, req.PGVersion)
	if err != nil {
		return LintResult{}, err
	}
	classified, err := migration.ClassifyStatements(ctx,
		catalogread.New(t.pool, catalogread.Default()), req.SQL, version)
	if err != nil {
		t.opts.Log("warn", "lint_migration: %v; the type change is kept as a rewrite", err)
	}
	assessor := migration.NewRiskAssessor(t.pool, t.opts.Log)
	res := LintResult{Statements: []LintItem{}}
	for _, c := range classified.All() {
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
	return reviewUnproven(res, classified), nil
}

// reviewUnproven raises a safe verdict to review when a statement is
// unrecognized (no DDL safety rule matched it and it is not a known safe
// form, migration-04) or when the text cannot be tokenized: safe must mean
// every statement is known. It never lowers a verdict or changes the
// score.
func reviewUnproven(res LintResult, classified migration.Classification) LintResult {
	if classified.Untokenizable {
		return raiseToReview(res, "The SQL could not be tokenized: it ends inside a "+
			"literal, quoted identifier, dollar-quoted body or comment, which can hide "+
			"statements. Review it.")
	}
	n := classified.Unrecognized()
	if n == 0 {
		return res
	}
	known := "a known safe form"
	if classified.Degraded {
		known = "on the additive allowlist (this build has no SQL parser)"
	}
	if n == 1 {
		return raiseToReview(res, "1 statement is not "+known+" and matches no DDL "+
			"safety rule: unrecognized DDL is not proven safe. Review it.")
	}
	return raiseToReview(res, fmt.Sprintf("%d statements are not %s and match no DDL "+
		"safety rule: unrecognized DDL is not proven safe. Review them.", n, known))
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

// Supported server majors for pg_version.
const (
	minLintMajor = 14
	maxLintMajor = 18
)

// normalizeServerVersion maps a requested pg_version to server_version_num:
// a major from 14 to 18 is scaled by 10000, a server_version_num from
// 140000 to 189999 passes, and anything else is an invalid request (0 is
// not a version; the caller reads the server's instead).
func normalizeServerVersion(v int) (int, error) {
	switch {
	case v >= minLintMajor && v <= maxLintMajor:
		return v * 10000, nil
	case v >= minLintMajor*10000 && v < (maxLintMajor+1)*10000:
		return v, nil
	}
	return 0, invalid("pg_version %d is not supported: give a major from %d to %d "+
		"or a server_version_num from %d to %d", v, minLintMajor, maxLintMajor,
		minLintMajor*10000, (maxLintMajor+1)*10000-1)
}

func (t *Tools) lintVersion(ctx context.Context, requested int) (int, error) {
	if requested != 0 {
		return normalizeServerVersion(requested)
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
