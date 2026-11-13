package agenttools

import (
	"context"
	"errors"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/migration"
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
	// PGVersion and RowThreshold are what the review used (migration-11).
	PGVersion    int   `json:"pg_version"`
	RowThreshold int64 `json:"row_threshold"`
	// Review is the shared engine's full per-statement report.
	Review migration.Report `json:"review"`
}

// LintItem is one rule a statement matched, scored on live statistics.
// The lock_timeout rule is an annotation: it is listed with no score.
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
	// StatementIndex is the 1-based statement the item belongs to.
	StatementIndex int    `json:"statement_index"`
	Verdict        string `json:"verdict,omitempty"`
	Annotation     bool   `json:"annotation,omitempty"`
	GuardedRewrite string `json:"guarded_rewrite,omitempty"`
}

// LintMigration lints migration SQL with the shared pre-merge review
// (migration.Review): the advisor's scoring path, its configured row
// threshold, the binary-coercible ALTER COLUMN TYPE check and the
// lock_timeout annotation. The SQL is text to analyze: it is never run.
// A catalog read that fails is an error, never a verdict.
func (t *Tools) LintMigration(ctx context.Context, req LintRequest) (LintResult, error) {
	if err := t.ready(); err != nil {
		return LintResult{}, err
	}
	if strings.TrimSpace(req.SQL) == "" {
		return LintResult{}, invalid("sql is required")
	}
	if len(req.SQL) > migration.MaxReviewBytes {
		return LintResult{}, invalid("sql is longer than %d bytes", migration.MaxReviewBytes)
	}
	if req.PGVersion != 0 {
		if _, err := normalizeServerVersion(req.PGVersion); err != nil {
			return LintResult{}, err
		}
	}
	rep, err := migration.Review(ctx, catalogread.New(t.pool, catalogread.Default()), req.SQL,
		migration.ReviewOptions{PGVersion: req.PGVersion,
			RowThreshold: int64(t.opts.DDLRowThreshold), Log: t.opts.Log})
	if errors.Is(err, migration.ErrInvalidMigration) {
		return LintResult{}, invalid("%v", err)
	}
	if err != nil {
		return LintResult{}, err
	}
	return lintResultOf(rep), nil
}

// lintResultOf is the report in lint_migration's established shape.
func lintResultOf(rep migration.Report) LintResult {
	res := LintResult{Statements: []LintItem{}, MaxRiskScore: rep.MaxRiskScore,
		Verdict: rep.Verdict, Note: strings.Join(rep.Notes, " "), PGVersion: rep.PGVersion,
		RowThreshold: rep.RowThreshold, Review: rep}
	for _, st := range rep.Statements {
		for _, h := range st.Hazards {
			res.Statements = append(res.Statements, LintItem{Statement: st.Statement,
				RuleID: h.RuleID, LockLevel: h.LockLevel, RequiresRewrite: h.RequiresRewrite,
				Table: h.Table, TableSizeBytes: h.TableSizeBytes, EstimatedRows: h.LiveRows,
				RiskScore: h.RiskScore, EstimatedLockMs: h.EstimatedLockMs,
				SafeAlternative: h.SafeAlternative, Description: h.Description,
				StatementIndex: st.Index, Verdict: h.Verdict, GuardedRewrite: st.GuardedRewrite})
		}
		for _, a := range st.Annotations {
			res.Statements = append(res.Statements, LintItem{Statement: st.Statement,
				RuleID: a.RuleID, Table: a.Table, SafeAlternative: a.SafeAlternative,
				Description: a.Description, StatementIndex: st.Index, Annotation: true})
		}
	}
	return res
}

// normalizeServerVersion maps a requested pg_version to server_version_num
// (migration.NormalizePGVersion); anything unsupported is an invalid
// request whose message names both accepted forms.
func normalizeServerVersion(v int) (int, error) {
	n, err := migration.NormalizePGVersion(v)
	if err != nil {
		return 0, invalid("%s", strings.TrimPrefix(err.Error(),
			migration.ErrInvalidMigration.Error()+": "))
	}
	return n, nil
}
