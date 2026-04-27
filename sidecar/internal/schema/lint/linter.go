package lint

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Linter orchestrates periodic schema anti-pattern detection.
type Linter struct {
	pool      *pgxpool.Pool
	rules     []Rule
	cfg       *config.SchemaLintConfig
	pgVer     int
	logFn     func(string, string, ...any)
	llmClient *llm.Client
}

// SetLLMClient sets the optional LLM client for enhanced analysis.
func (l *Linter) SetLLMClient(c *llm.Client) { l.llmClient = c }

// New creates a Linter with the default rule set.
func New(
	pool *pgxpool.Pool,
	cfg *config.SchemaLintConfig,
	pgVer int,
	logFn func(string, string, ...any),
) *Linter {
	l := &Linter{
		pool:  pool,
		cfg:   cfg,
		pgVer: pgVer,
		logFn: logFn,
	}
	l.rules = defaultRules()
	return l
}

// Scan runs all enabled rules and returns findings.
func (l *Linter) Scan(ctx context.Context) ([]Finding, error) {
	disabled := make(map[string]bool, len(l.cfg.DisabledRules))
	for _, id := range l.cfg.DisabledRules {
		disabled[id] = true
	}

	opts := RuleOpts{
		MinTableRows:   l.cfg.MinTableRows,
		PGVersionNum:   l.pgVer,
		ExcludeSchemas: l.cfg.ExcludeSchemas,
	}
	if opts.MinTableRows == 0 {
		opts.MinTableRows = 1000
	}

	var all []Finding
	for _, r := range l.rules {
		if disabled[r.ID()] {
			continue
		}
		start := time.Now()
		findings, err := r.Check(ctx, l.pool, opts)
		elapsed := time.Since(start)
		if err != nil {
			l.logFn("warn", "lint rule %s failed: %v", r.ID(), err)
			continue
		}
		if len(findings) > 0 {
			l.logFn("info", "lint rule %s: %d findings (%s)",
				r.ID(), len(findings), elapsed.Truncate(time.Millisecond))
		}
		all = append(all, filterIncludedSchemas(findings, l.cfg.IncludeSchemas)...)
	}

	if l.llmClient != nil {
		enhancer := &LLMJsonbAnalyzer{
			pool: l.pool, llmClient: l.llmClient, logFn: l.logFn,
		}
		all = enhancer.Enhance(ctx, all)
	}

	return all, nil
}

func filterIncludedSchemas(findings []Finding, include []string) []Finding {
	if len(include) == 0 || len(findings) == 0 {
		return findings
	}
	allowed := make(map[string]bool, len(include))
	for _, schema := range include {
		allowed[schema] = true
	}
	out := findings[:0]
	for _, f := range findings {
		if allowed[f.Schema] {
			out = append(out, f)
		}
	}
	return out
}

// defaultRules returns the built-in rule set.
//
// Scope: schema lint is responsible ONLY for design/convention/
// correctness issues — things that cannot be derived from runtime
// stats. Runtime-stats-driven rules (unused/duplicate/invalid/
// bloat/missing-FK) are owned by internal/analyzer, which runs on
// a shorter cadence (seconds, not an hour) against the collector
// snapshot. Producing the same finding in two places led to
// duplicate dashboard rows and split ownership.
//
// The following rule types still exist in this package
// (ruleUnusedIndex, ruleDuplicateIndex, ruleInvalidIndex,
// ruleMissingFKIndex, ruleBloatedTable) but are intentionally
// NOT registered here. They are kept around only because their
// integration tests exercise valuable SQL against a live DB; do
// not re-add them to defaults without first removing the analyzer
// equivalents.
func defaultRules() []Rule {
	return []Rule{
		// Safety rules — conditions the analyzer does not track.
		&ruleSequenceOverflow{},
		&ruleNoPrimaryKey{},
		&ruleTxidAge{},
		&ruleMxidAge{},
		// Performance rules — pure schema-shape, not runtime stats.
		&ruleOverlappingIndex{},
		&ruleToastHeavy{},
		&ruleJsonbInJoins{},
		&ruleWideTable{},
		&ruleLowCardinalityIndex{},
		// Correctness rules — type/nullability design issues.
		&ruleTimestampNoTZ{},
		&ruleCharUsage{},
		&ruleFKTypeMismatch{},
		&ruleNullableUnique{},
		// Convention rules — style / migration posture.
		&ruleSerialUsage{},
		&ruleVarchar255{},
		&ruleIntPK{},
	}
}
