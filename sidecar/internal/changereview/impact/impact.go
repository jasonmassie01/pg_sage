// Package impact is Change Review's deterministic engine: it implements
// changereview.Analyzer (one review's SQL) and changereview.Inspector
// (one statement, the reasoner's inspect_statement tool).
//
// For every statement it inspects the SQL with libpg_query (or, in a
// build without it or for SQL it rejects, the regex classifier in text
// mode), loads bounded live context for each touched relation, builds the
// workload index from pg_stat_statements and sage.query_store, and runs
// rules CR-01..CR-18. Every finding cites evidence with sha256 digests;
// unknown is reported as unknown (CR-17), never as none.
//
// It only reads: catalog and statistics reads run in bounded READ ONLY
// transactions (catalogread), reads and the read shapes of writes are
// planned plan-only (internal/explain), and index what-ifs are HypoPG's
// session-local hypothetical indexes. The reviewed SQL is never run.
package impact

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// WhatIf measures a hypothetical index (*optimizer.HypoPG satisfies it).
type WhatIf interface {
	IsAvailable(ctx context.Context) bool
	Validate(ctx context.Context, rec optimizer.Recommendation,
		queries []optimizer.QueryInfo) (optimizer.WhatIfResult, error)
}

// Deps are the engine's dependencies. Pool is the monitored database
// (required). Config holds the thresholds InspectStatement uses (Analyze
// uses its Input's). Timeouts bound each catalog read (zero:
// catalogread.Default()); ExplainTimeout bounds each plan-only EXPLAIN
// (zero: 3 s). WhatIf nil uses optimizer.NewHypoPG on Pool.
type Deps struct {
	Pool           *pgxpool.Pool
	Config         config.ChangeReviewConfig
	Timeouts       catalogread.Timeouts
	ExplainTimeout time.Duration
	WhatIf         WhatIf
	Now            func() time.Time
	Logf           func(format string, args ...any)
}

const defaultExplainTimeout = 3 * time.Second

// Engine is the impact engine of one monitored database. It is safe for
// concurrent use: each Analyze or InspectStatement call has its own state.
type Engine struct {
	d Deps
}

// The parser seams (tests run the engine as a build without libpg_query).
var (
	astAvailable = sqlast.Available
	inspectDDL   = sqlast.InspectDDL
)

// New returns the engine, or ErrInvalid without a pool.
func New(d Deps) (*Engine, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("%w: the impact engine needs a database pool",
			changereview.ErrInvalid)
	}
	if d.Timeouts == (catalogread.Timeouts{}) {
		d.Timeouts = catalogread.Default()
	}
	if d.ExplainTimeout <= 0 {
		d.ExplainTimeout = defaultExplainTimeout
	}
	if d.WhatIf == nil {
		d.WhatIf = optimizer.NewHypoPG(d.Pool, func(string, string, ...any) {})
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Config == (config.ChangeReviewConfig{}) {
		d.Config = config.DefaultChangeReviewConfig()
	}
	return &Engine{d: d}, nil
}

// Analyze is the deterministic review of in's SQL. It returns an error
// only when ctx ends (the runner's 20 s deadline or a cancellation); a
// read that fails or times out becomes CR-17 evidence instead.
func (e *Engine) Analyze(ctx context.Context, in changereview.Input) (
	changereview.Analysis, error) {
	if err := ctx.Err(); err != nil {
		return changereview.Analysis{}, fmt.Errorf("impact analysis: %w", err)
	}
	r := e.newRun(ctx, in.Config, in)
	r.readVersion()
	r.analyzeStatements(r.parseFiles(in.SQLFiles))
	if r.err != nil {
		return changereview.Analysis{}, fmt.Errorf("impact analysis: %w", r.err)
	}
	return r.analysis(), nil
}

// analyzeStatements runs stages 2-7 over the parsed statements.
func (r *run) analyzeStatements(texts []textFile) {
	r.loadRelations()
	if r.err != nil {
		return
	}
	r.tableEvidence()
	for _, s := range r.stmts {
		r.statementEvidence(s)
	}
	r.textModeFindings(texts)
	r.loadWorkload()
	for _, s := range r.stmts {
		if r.err != nil {
			return
		}
		r.runRules(s)
	}
}

func (e *Engine) logf(format string, args ...any) { e.d.Logf(format, args...) }

// qualified renders a name "schema.name" (schema resolved).
func qualified(n sqlast.QualifiedName) string {
	if n.Schema == "" {
		return n.Name
	}
	return n.Schema + "." + n.Name
}

// splitQualified splits "schema.name" at its first dot.
func splitQualified(rel string) (string, string, bool) {
	schema, name, ok := strings.Cut(rel, ".")
	return schema, name, ok && schema != "" && name != ""
}
