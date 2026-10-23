// Package crfake holds Change Review test stages and fixtures shared by
// the surfaces' tests (REST, MCP, CLI, notify): a deterministic analyzer
// whose verdict follows markers in the reviewed SQL, a reasoner that
// reports a fixed model status, and a live-Postgres service fixture.
// Nothing here reaches an LLM provider or runs the reviewed SQL.
package crfake

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Markers in the reviewed SQL that choose the fake analyzer's findings:
// "-- risky" yields a critical CR-03 finding, "-- review" a warning CR-01
// finding, anything else an info CR-15 finding only (verdict safe).
const (
	MarkRisky  = "-- risky"
	MarkReview = "-- review"
)

// Analyzer is a deterministic stand-in for the impact engine. When Gate is
// set, each analysis blocks until Gate is closed or receives a value (or
// the review's context ends); Entered then receives one value per analysis
// that started.
type Analyzer struct {
	Gate    chan struct{}
	Entered chan struct{}
	calls   atomic.Int32
}

// Calls is how many analyses started.
func (a *Analyzer) Calls() int { return int(a.calls.Load()) }

// Analyze answers one statement per SQL input, with findings chosen by
// the markers.
func (a *Analyzer) Analyze(ctx context.Context, in changereview.Input) (
	changereview.Analysis, error) {
	a.calls.Add(1)
	if a.Entered != nil {
		a.Entered <- struct{}{}
	}
	if a.Gate != nil {
		select {
		case <-a.Gate:
		case <-ctx.Done():
			return changereview.Analysis{}, ctx.Err()
		}
	}
	out := changereview.Analysis{ParseMode: changereview.ParseAST}
	for i, f := range in.SQLFiles {
		ev, err := changereview.NewEvidence("stmt", strconv.Itoa(i+1),
			"statement "+strconv.Itoa(i+1), map[string]any{"sql": f.Content})
		if err != nil {
			return changereview.Analysis{}, err
		}
		out.Evidence = append(out.Evidence, ev)
		out.Statements = append(out.Statements, changereview.Statement{Index: i + 1,
			Origin: changereview.OriginSQLInput, File: f.Path, Line: 1, SQL: f.Content,
			Kind: "other", Relations: []string{"public.orders"}, EvidenceID: ev.ID})
		out.Findings = append(out.Findings, findingFor(f.Content, ev.ID))
	}
	out.Expectations = changereview.Expectations{Relations: []string{"public.orders"}}
	return out, nil
}

func findingFor(sql, evidence string) changereview.Finding {
	rule, kind, sev := "CR-15", "new_object_antipattern", changereview.SeverityInfo
	switch {
	case strings.Contains(sql, MarkRisky):
		rule, kind, sev = "CR-03", "column_still_referenced", changereview.SeverityCritical
	case strings.Contains(sql, MarkReview):
		rule, kind, sev = "CR-01", "lock_and_rewrite", changereview.SeverityWarning
	}
	return changereview.Finding{RuleID: rule, Kind: kind, Severity: sev,
		Origin: changereview.OriginRule, Title: rule + " " + sev,
		Text: "fake " + kind + " finding", EvidenceIDs: []string{evidence}}
}

// Reasoner reports Status as the model status and adds nothing.
type Reasoner struct{ Status string }

// Reason returns the fixed model status.
func (r Reasoner) Reason(context.Context, changereview.Input, changereview.Analysis) (
	changereview.Reasoning, error) {
	return changereview.Reasoning{ModelStatus: r.Status}, nil
}

var (
	bootstrapOnce sync.Once
	bootstrapErr  error
)

// Pool connects to the package's disposable test database (skipping t
// when none is configured), bootstraps the sage schema once per process
// and empties the Change Review tables.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	bootstrapOnce.Do(func() { bootstrapErr = schema.Bootstrap(ctx, pool) })
	if bootstrapErr != nil {
		t.Fatalf("bootstrap sage schema: %v", bootstrapErr)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE sage.change_review,
		sage.change_review_budget_day`); err != nil {
		t.Fatalf("empty change review tables: %v", err)
	}
	return pool
}

// Config is the default Change Review configuration.
func Config() config.ChangeReviewConfig { return config.DefaultChangeReviewConfig() }

// Start builds and starts a service for database on pool with analyzer a
// (and the other deps from mutate, when set); the runner stops when the
// test ends.
func Start(t testing.TB, pool *pgxpool.Pool, database string, a changereview.Analyzer,
	mutate func(*changereview.Deps)) *changereview.Service {
	t.Helper()
	d := changereview.Deps{Database: database, Pool: pool, Config: Config(), Analyzer: a,
		Logf: Logf(t)}
	if mutate != nil {
		mutate(&d)
	}
	svc, err := changereview.New(d)
	if err != nil {
		t.Fatalf("new change review service: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := svc.Start(ctx); err != nil {
		t.Fatalf("start change review service: %v", err)
	}
	return svc
}

// Logf logs to t until the test ends and drops lines after that, so a
// runner goroutine finishing late cannot log into a completed test.
func Logf(t testing.TB) func(format string, args ...any) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		done = true
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}
