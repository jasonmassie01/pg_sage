package impact

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Lint bars of agenttools.LintMigration, which CR-01 reuses.
const (
	riskyScore  = 0.7
	reviewScore = 0.3
)

// assessment is one classifier rule a statement matched, scored by
// migration.RiskAssessor.
type assessment struct {
	risk     *migration.DDLRisk
	coercion bool
	evID     string
}

// lockRules is CR-01 for one statement: the regex classifier's rules
// scored by RiskAssessor, with the binary-coercible refinement for type
// changes. Settings are tracked by the parser, so the classifier's own
// lock_timeout rule is left to CR-02. Findings also cite the statement's
// expand/contract steps. It returns the assessments.
func (r *run) lockRules(s *stmt, expand []string) []assessment {
	classes := s.classes
	if !s.text {
		classes = migration.NewRegexClassifier().Classify(s.sql, r.version)
	}
	var out []assessment
	for i, c := range classes {
		if c.RuleID == "ddl_missing_lock_timeout" {
			continue
		}
		coerced := r.refineType(s, &c)
		a := assessment{risk: r.assess(c), coercion: coerced}
		a.evID = r.riskEvidence(s, i, a)
		out = append(out, a)
		r.find(RuleLockAndRewrite, lintSeverity(a.risk), lockTitle(a.risk),
			a.risk.Description+lockSuffix(a.risk), append([]string{s.evID, a.evID},
				expand...)...)
	}
	return out
}

// refineType marks a type change PostgreSQL does without a rewrite
// (varchar widening, varchar to text: migration/alter_type.go). A USING
// expression other than the column itself makes PostgreSQL compute every
// row again: a rewrite whatever the types.
func (r *run) refineType(s *stmt, c *migration.DDLClassification) bool {
	if c.RuleID != "ddl_alter_type_rewrite" || s.text {
		return false
	}
	coercible := false
	for i, p := range s.parts {
		if p.Kind != sqlast.DDLAlterType || p.Column == nil {
			continue
		}
		cur, ok := r.currentType(s, i, p.Column.Name)
		if !ok || !usingOnlyTheColumn(p.Column) ||
			!migration.IsBinaryCoercibleChange(cur, formatType(p.Column.Type)) {
			return false
		}
		coercible = true
	}
	if coercible {
		c.RequiresRewrite = false
		c.Description = "Binary-coercible type change: no rewrite, but ACCESS EXCLUSIVE " +
			"is still taken"
	}
	return coercible
}

// usingOnlyTheColumn reports whether an ALTER TYPE has no USING clause or
// one that is just the column (quoted or not).
func usingOnlyTheColumn(c *sqlast.ColumnDef) bool {
	u := strings.TrimSpace(c.Using)
	return u == "" || u == c.Name || u == `"`+strings.ReplaceAll(c.Name, `"`, `""`)+`"`
}

// currentType is the catalog type of a column of part i's table.
func (r *run) currentType(s *stmt, i int, col string) (string, bool) {
	rel := r.target(s, i)
	if rel == nil || !rel.found {
		return "", false
	}
	c, ok := rel.columns[col]
	return c.typ, ok
}

func (r *run) assess(c migration.DDLClassification) *migration.DDLRisk {
	ra := migration.NewRiskAssessor(r.e.d.Pool, func(level, format string, args ...any) {
		r.e.logf("change review impact: risk assessor %s: "+format,
			append([]any{level}, args...)...)
	})
	ra.SetCatalogReadTimeouts(r.e.d.Timeouts)
	risk, err := ra.Assess(r.ctx, c)
	if err != nil || risk == nil {
		r.readFailed(err, nil, "risk of "+c.TableName)
		return &migration.DDLRisk{RuleID: c.RuleID, LockLevel: c.LockLevel,
			RequiresRewrite: c.RequiresRewrite, Description: c.Description}
	}
	return risk
}

func (r *run) riskEvidence(s *stmt, i int, a assessment) string {
	k := a.risk
	return r.addEvidence("risk", partRef(s.index, i), fmt.Sprintf("%s: %s, score %.2f",
		k.RuleID, k.LockLevel, k.RiskScore), riskData{RuleID: k.RuleID,
		LockLevel: k.LockLevel, RequiresRewrite: k.RequiresRewrite,
		BinaryCoercible: a.coercion, RiskScore: k.RiskScore,
		EstimatedLockMs: k.EstimatedLockMs, TableBytes: k.TableSizeBytes,
		EstimatedRows: k.EstimatedRows, SafeAlternative: k.SafeAlternative,
		Description: k.Description})
}

// lintSeverity is LintMigration's verdict as a severity: risky at 0.7,
// review at 0.3 or for any rewrite or ACCESS EXCLUSIVE lock.
func lintSeverity(k *migration.DDLRisk) string {
	switch {
	case k.RiskScore >= riskyScore:
		return changereview.SeverityCritical
	case k.RiskScore >= reviewScore || k.RequiresRewrite ||
		k.LockLevel == lockAccessExclusive:
		return changereview.SeverityWarning
	}
	return changereview.SeverityInfo
}

func lockTitle(k *migration.DDLRisk) string {
	t := "Takes " + k.LockLevel
	if k.LockLevel == "" {
		t = "DDL hazard " + k.RuleID
	}
	if k.RequiresRewrite {
		t += " and rewrites the table"
	}
	return t
}

func lockSuffix(k *migration.DDLRisk) string {
	s := fmt.Sprintf(" (risk score %.2f, about %d ms under the lock", k.RiskScore,
		k.EstimatedLockMs)
	if k.TableSizeBytes > 0 {
		s += fmt.Sprintf(", %d bytes", k.TableSizeBytes)
	}
	s += ")."
	if k.SafeAlternative != "" {
		s += " Safer: " + k.SafeAlternative + "."
	}
	return s
}

// holdMs is the lock hold of a statement: RiskAssessor's estimate
// (the largest of its assessments, or of the lock alone).
func (r *run) holdMs(s *stmt, i int, mode string, as []assessment) int64 {
	var hold int64
	for _, a := range as {
		hold = max(hold, a.risk.EstimatedLockMs)
	}
	if hold > 0 {
		return hold
	}
	rel := r.target(s, i)
	c := migration.DDLClassification{LockLevel: mode, Statement: s.sql}
	if rel != nil {
		c.SchemaName, c.TableName = rel.schema, rel.rel
	}
	return r.assess(c).EstimatedLockMs
}

// tableLock is a lock a statement takes on one relation.
type tableLock struct{ rel, mode string }

// conflicting is the workload statements on the locked relations whose
// lock conflicts with the mode taken there, sorted by queryid.
func (r *run) conflicting(locks ...tableLock) []*workStmt {
	seen := map[int64]bool{}
	var out []*workStmt
	for _, l := range locks {
		for _, ws := range r.work.byRelation[l.rel] {
			if !seen[ws.id] && conflicts(l.mode, ws.lockMode()) {
				seen[ws.id] = true
				out = append(out, ws)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}
