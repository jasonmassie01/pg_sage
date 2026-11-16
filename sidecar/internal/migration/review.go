package migration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Review is the one pre-merge migration review (migration-11): MCP
// lint_migration, POST /api/v1/migrations/lint and the lint-migration CLI
// all call it, and it scores with the advisor's own path
// (scoreClassifications), so a migration reads the same before it runs as
// the advisor reports it while it runs.

// Verdicts, mildest first.
const (
	VerdictSafe   = "safe"
	VerdictReview = "review"
	VerdictRisky  = "risky"
)

// MaxReviewBytes is the largest migration text Review accepts (inclusive).
const MaxReviewBytes = 100000

// Review errors. ErrInvalidMigration is the caller's input (400, CLI exit
// 3); ErrCatalogUnavailable is a catalog read that failed or timed out
// (503, CLI exit 3): a migration that could not be measured is never
// reported safe.
var (
	ErrInvalidMigration   = errors.New("invalid migration")
	ErrCatalogUnavailable = errors.New("catalog read failed")
)

// oldestSupportedVersion is the server_version_num a static review with no
// --pg-version classifies for: a rule that only bites on older majors is
// never missed.
const oldestSupportedVersion = 140000

// ReviewOptions configure one review.
type ReviewOptions struct {
	// PGVersion is a major (14-18) or a server_version_num; 0 reads the
	// server's (static: the oldest supported major).
	PGVersion int
	// RowThreshold is migration.ddl_row_threshold; 0 is the default.
	RowThreshold int64
	// Static reviews with no connection: intrinsic hazards only, sizes
	// unknown. A nil querier is always static.
	Static bool
	// Log receives diagnostics; nil discards them.
	Log func(level, format string, args ...any)
}

// Report is one migration's review.
type Report struct {
	Verdict      string            `json:"verdict"`
	MaxRiskScore float64           `json:"max_risk_score"`
	Static       bool              `json:"static"`
	PGVersion    int               `json:"pg_version"`
	RowThreshold int64             `json:"row_threshold"`
	Statements   []StatementReview `json:"statements"`
	Notes        []string          `json:"notes,omitempty"`
}

// StatementReview is one statement of the migration.
type StatementReview struct {
	Index     int             `json:"index"` // 1-based
	Line      int             `json:"line"`  // 1-based start line; 0 when unknown
	Statement string          `json:"statement"`
	Status    StatementStatus `json:"status"`
	Verdict   string          `json:"verdict"`
	// LockTimeoutMissing is the advisor's annotation: the statement takes
	// ACCESS EXCLUSIVE and no earlier statement set lock_timeout.
	LockTimeoutMissing bool `json:"lock_timeout_missing"`
	// Annotations is advice that is not scored (the lock_timeout rule).
	Annotations []Annotation `json:"annotations,omitempty"`
	Hazards     []Hazard     `json:"hazards"`
	// GuardedRewrite is a deterministic safer form of the statement.
	GuardedRewrite string `json:"guarded_rewrite,omitempty"`
	Note           string `json:"note,omitempty"`
}

// Annotation is a rule's advice that carries no score of its own.
type Annotation struct {
	RuleID          string `json:"rule_id"`
	Table           string `json:"table,omitempty"`
	Description     string `json:"description"`
	SafeAlternative string `json:"safe_alternative"`
}

// Hazard is one DDL safety rule a statement matched, scored on live
// statistics (or intrinsically, in static mode).
type Hazard struct {
	RuleID          string  `json:"rule_id"`
	LockLevel       string  `json:"lock_level,omitempty"`
	RequiresRewrite bool    `json:"requires_rewrite"`
	Table           string  `json:"table,omitempty"`
	SizeKnown       bool    `json:"size_known"`
	Size            string  `json:"size"` // "unknown" or "<rows> rows, <bytes> bytes"
	LiveRows        int64   `json:"live_rows"`
	TableSizeBytes  int64   `json:"table_size_bytes"`
	RiskScore       float64 `json:"risk_score"`
	EstimatedLockMs int64   `json:"estimated_lock_ms"`
	Verdict         string  `json:"verdict"`
	SafeAlternative string  `json:"safe_alternative,omitempty"`
	Description     string  `json:"description,omitempty"`
}

// Review classifies sql (text to analyze: it is never run), scores every
// hazard with the advisor's path and the configured row threshold, and
// returns the verdict. q runs the bounded catalog reads; nil or o.Static
// reviews statically. A failed read returns the report and an error
// wrapping ErrCatalogUnavailable.
func Review(ctx context.Context, q catalogread.Querier, sql string,
	o ReviewOptions) (Report, error) {
	if strings.TrimSpace(sql) == "" {
		return Report{}, fmt.Errorf("%w: the migration is empty", ErrInvalidMigration)
	}
	if len(sql) > MaxReviewBytes {
		return Report{}, fmt.Errorf("%w: the migration is %d bytes, over the %d-byte limit",
			ErrInvalidMigration, len(sql), MaxReviewBytes)
	}
	if o.Static {
		q = nil
	}
	version, err := reviewVersion(ctx, q, o.PGVersion)
	if err != nil {
		return Report{}, err
	}
	threshold := o.RowThreshold
	if threshold <= 0 {
		threshold = defaultDDLRowThreshold
	}
	cls, _ := ClassifyStatements(ctx, nil, sql, version) // no catalog: no error
	ra := newQuerierAssessor(q, threshold, reviewLog(o.Log))
	rep := Report{Static: q == nil, PGVersion: version, RowThreshold: threshold,
		Statements: []StatementReview{}}
	lines := statementLines(sql, len(cls.Statements))
	var errs []error
	for i, st := range cls.Statements {
		sr, err := reviewStatement(ctx, ra, st, rep.Static)
		sr.Index, sr.Line = i+1, lines[i]
		errs = append(errs, err)
		rep.Statements = append(rep.Statements, sr)
	}
	finishReport(&rep, cls)
	if err := errors.Join(errs...); err != nil {
		return rep, fmt.Errorf("%w: %v", ErrCatalogUnavailable, err)
	}
	return rep, nil
}

// reviewVersion normalizes the requested version, reads the server's
// when none is given, and is the oldest supported major statically.
func reviewVersion(ctx context.Context, q catalogread.Querier, requested int) (int, error) {
	if requested != 0 {
		return NormalizePGVersion(requested)
	}
	if q == nil {
		return oldestSupportedVersion, nil
	}
	var version int
	err := q.QueryRow(ctx,
		"/* pg_sage */ SELECT current_setting('server_version_num')::int").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("%w: read server version: %v", ErrCatalogUnavailable, err)
	}
	return version, nil
}

// Supported server majors.
const (
	minSupportedMajor = 14
	maxSupportedMajor = 18
)

// NormalizePGVersion maps a requested version to server_version_num: a
// major from 14 to 18 is scaled by 10000, a server_version_num from 140000
// to 189999 passes, and anything else wraps ErrInvalidMigration.
func NormalizePGVersion(v int) (int, error) {
	switch {
	case v >= minSupportedMajor && v <= maxSupportedMajor:
		return v * 10000, nil
	case v >= minSupportedMajor*10000 && v < (maxSupportedMajor+1)*10000:
		return v, nil
	}
	return 0, fmt.Errorf("%w: pg_version %d is not supported: give a major from %d to %d "+
		"or a server_version_num from %d to %d", ErrInvalidMigration, v, minSupportedMajor,
		maxSupportedMajor, minSupportedMajor*10000, (maxSupportedMajor+1)*10000-1)
}

func reviewLog(fn func(string, string, ...any)) func(string, string, ...any) {
	if fn == nil {
		return func(string, string, ...any) {}
	}
	return fn
}

// reviewStatement scores one classified statement. Its error joins the
// catalog reads that failed (the statement is still scored from what is
// known).
func reviewStatement(ctx context.Context, ra *RiskAssessor, st ClassifiedStatement,
	static bool) (StatementReview, error) {
	sr := StatementReview{Statement: st.Statement, Status: st.Status,
		Verdict: VerdictSafe, Hazards: []Hazard{}}
	var errs []error
	refine := func(c *DDLClassification) {
		if ra.live() {
			errs = append(errs, refineAlterType(ctx, ra.catalog(), c))
		}
	}
	risks, lockTimeout, err := scoreClassifications(ctx, ra, st.Classifications, refine)
	errs = append(errs, err)
	if lockTimeout != nil {
		sr.LockTimeoutMissing = true
		sr.Annotations = []Annotation{{RuleID: lockTimeout.RuleID,
			Table:       qualifiedTable(lockTimeout.SchemaName, lockTimeout.TableName),
			Description: lockTimeout.Description, SafeAlternative: lockTimeout.SafeAlternative}}
	}
	for _, r := range risks {
		errs = append(errs, r.readErr)
		h := hazardOf(r, static)
		h.Verdict = hazardVerdict(r, ra.rowThreshold, !sr.LockTimeoutMissing)
		sr.Verdict = worstVerdict(sr.Verdict, h.Verdict)
		sr.Hazards = append(sr.Hazards, h)
	}
	if st.Status == StatementUnrecognized {
		sr.Verdict = worstVerdict(sr.Verdict, VerdictReview)
		sr.Note = "No DDL safety rule matches this statement and it is not a known " +
			"safe form: unrecognized DDL is not proven safe."
		if msg := parseRefusal(st.Statement); msg != "" {
			sr.Note += " The SQL parser (" + parserGrammar + " grammar) refused it: " +
				msg + "."
		}
	}
	sr.GuardedRewrite, sr.Note = guardedRewrite(sr, risks, sr.Note)
	return sr, errors.Join(errs...)
}

// scoreClassifications assesses every classification except the
// lock_timeout annotation (no intrinsic hazard of its own, G7-B33), which
// it returns: the one scoring path of the advisor and Review. refine runs
// on each copy first (the ALTER COLUMN TYPE catalog check).
func scoreClassifications(ctx context.Context, ra *RiskAssessor, cs []DDLClassification,
	refine func(*DDLClassification)) ([]*DDLRisk, *DDLClassification, error) {
	var risks []*DDLRisk
	var lockTimeout *DDLClassification
	var errs []error
	for i := range cs {
		if cs[i].RuleID == lockTimeoutRuleID {
			lockTimeout = &cs[i]
			continue
		}
		c := cs[i]
		refine(&c)
		risk, err := ra.Assess(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("risk assessment failed for %s: %w", c.RuleID, err))
			continue
		}
		risks = append(risks, risk)
	}
	return risks, lockTimeout, errors.Join(errs...)
}

// lockTimeoutRuleID is the batch annotation's rule.
const lockTimeoutRuleID = "ddl_missing_lock_timeout"

// qualifiedTable is schema.table as written ("" without a table).
func qualifiedTable(schema, table string) string {
	if schema != "" && table != "" {
		return schema + "." + table
	}
	return table
}

func hazardOf(r *DDLRisk, static bool) Hazard {
	h := Hazard{RuleID: r.RuleID, LockLevel: r.LockLevel, RequiresRewrite: r.RequiresRewrite,
		Table: qualifiedTable(r.SchemaName, r.TableName), Size: "unknown", RiskScore: r.RiskScore,
		EstimatedLockMs: r.EstimatedLockMs, SafeAlternative: r.SafeAlternative,
		Description: r.Description}
	if !static && r.StatsKnown {
		h.SizeKnown, h.LiveRows, h.TableSizeBytes = true, r.EstimatedRows, r.TableSizeBytes
		h.Size = fmt.Sprintf("%d rows, %d bytes", r.EstimatedRows, r.TableSizeBytes)
	}
	return h
}
