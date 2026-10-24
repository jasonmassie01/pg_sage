package reasoner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The reasoner inside the real Change Review service and store on a live
// PostgreSQL: what is stored after the model stage, the real daily budget
// under concurrency, and that the raw diff never reaches a column. The
// deterministic stage is a fixed analysis here (live_*_test.go uses the
// impact engine).

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/changereview/reasoner"))
}

var (
	bootstrapOnce sync.Once
	bootstrapErr  error
)

type dbFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func newDBFixture(t *testing.T) *dbFixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	bootstrapOnce.Do(func() { bootstrapErr = schema.Bootstrap(ctx, pool) })
	if bootstrapErr != nil {
		t.Fatalf("bootstrap sage schema: %v", bootstrapErr)
	}
	f := &dbFixture{t: t, ctx: ctx, pool: pool}
	if _, err := pool.Exec(ctx,
		`TRUNCATE sage.change_review, sage.change_review_budget_day`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return f
}

// fixedAnalyzer is a deterministic stage returning one analysis.
type fixedAnalyzer struct{ a changereview.Analysis }

func (f fixedAnalyzer) Analyze(context.Context, changereview.Input) (changereview.Analysis,
	error) {
	return f.a, nil
}

// service is the Change Review service with the reasoner r over the fixture,
// wired as production wires it (diffscan redaction and hunk filter).
func (f *dbFixture) service(a changereview.Analysis, r changereview.Reasoner,
	cfg config.ChangeReviewConfig) *changereview.Service {
	f.t.Helper()
	s, err := changereview.New(changereview.Deps{Database: "orders", Pool: f.pool,
		Config: cfg, Analyzer: fixedAnalyzer{a}, Reasoner: r, Redact: diffscan.Redact,
		HunkFilter: diffscan.HunkFilter, Logf: f.t.Logf})
	if err != nil {
		f.t.Fatalf("changereview.New: %v", err)
	}
	if _, err := s.Start(f.ctx); err != nil {
		f.t.Fatalf("Start: %v", err)
	}
	return s
}

var caller = changereview.Caller{Actor: "user:7", Source: changereview.SourceCLI}

func (f *dbFixture) submit(s *changereview.Service, req changereview.Request) changereview.Review {
	f.t.Helper()
	req.WaitSeconds = 45
	got, _, err := s.Submit(f.ctx, caller, req)
	if err != nil {
		f.t.Fatalf("Submit: %v", err)
	}
	if got.State != changereview.StateComplete {
		f.t.Fatalf("review %s is %s (%s), want complete", got.ID, got.State, got.FailureReason)
	}
	return got
}

func railsRequest(diff string) changereview.Request {
	return changereview.Request{Diff: diff, MigrationTool: "rails",
		PR: changereview.PRInfo{URL: "https://github.com/acme/shop/pull/12", Number: 12,
			Title: "Rename email"}}
}

func TestService_StoresTheModelStage(t *testing.T) {
	f := newDBFixture(t)
	fake := newFakeLLM(t, railsRenameRun(t)...)
	r := newTestReasoner(t, Deps{Model: fake.client()})
	got := f.submit(f.service(noSQLAnalysis(), r, config.DefaultChangeReviewConfig()),
		railsRequest(railsRenameDiff))
	var model []changereview.Finding
	for _, x := range got.Findings {
		if x.Origin == changereview.OriginModel {
			model = append(model, x)
		}
	}
	if got.ModelStatus != changereview.ModelOK || got.Verdict != changereview.VerdictReview ||
		len(model) != 1 || model[0].Severity != changereview.SeverityWarning ||
		model[0].Title != "billing-api still reads email" {
		t.Fatalf("stored %s/%s model findings %+v", got.ModelStatus, got.Verdict, model)
	}
	if !hasRule(got.Findings, changereview.RuleModelTranslated) || len(got.Statements) != 1 ||
		got.Statements[0].Origin != changereview.OriginModelTranslated {
		t.Fatalf("stored statements %+v findings %+v", got.Statements, got.Findings)
	}
	if !strings.Contains(got.Markdown, "2 model claims or findings were dropped") ||
		got.Summary.DroppedCount != 2 {
		t.Fatalf("dropped count: summary %d, markdown:\n%s", got.Summary.DroppedCount,
			got.Markdown)
	}
	if got.Transcript == nil || got.Transcript.Stop != agentloop.StopFinal || got.Tokens <= 0 {
		t.Fatalf("transcript %+v tokens %d", got.Transcript, got.Tokens)
	}
	for _, e := range got.Evidence {
		if !e.Verify() {
			t.Fatalf("stored evidence %s does not verify", e.ID)
		}
	}
}

func TestService_ProviderFailureKeepsTheDeterministicReview(t *testing.T) {
	f := newDBFixture(t)
	fake := newFakeLLM(t, statusReply(http.StatusTooManyRequests))
	r := newTestReasoner(t, Deps{Model: fake.client()})
	got := f.submit(f.service(riskyAnalysis(), r, config.DefaultChangeReviewConfig()),
		changereview.Request{SQL: "ALTER TABLE orders RENAME COLUMN email TO e"})
	if got.ModelStatus != changereview.ModelRateLimited || got.Verdict != changereview.VerdictRisky ||
		got.Transcript == nil || got.Transcript.Stop != agentloop.StopRateLimited {
		t.Fatalf("stored %s/%s transcript %+v", got.ModelStatus, got.Verdict, got.Transcript)
	}
}

func TestService_LLMOffSaysAppCodeWasNotAnalyzed(t *testing.T) {
	f := newDBFixture(t)
	got := f.submit(f.service(noSQLAnalysis(), newTestReasoner(t, Deps{}),
		config.DefaultChangeReviewConfig()), railsRequest(railsRenameDiff+controllerDiff))
	if got.ModelStatus != changereview.ModelNoModel || got.Summary.AppCodeAnalyzed ||
		!hasRule(got.Findings, changereview.RuleNotAnalyzableSQL) {
		t.Fatalf("stored %s findings %+v", got.ModelStatus, got.Findings)
	}
	if !strings.Contains(got.Markdown, "App-code changes were not analyzed") {
		t.Fatalf("markdown:\n%s", got.Markdown)
	}
}

func TestService_RawDiffNeverStored(t *testing.T) {
	f := newDBFixture(t)
	fake := newFakeLLM(t,
		calls(toolCall{ToolDiffExcerpt, jsonArgs(map[string]any{"path": secretMigration,
			"from_line": 1, "to_line": 9})}),
		submit(func(body string) map[string]any {
			return map[string]any{"findings": []any{map[string]any{"severity": "warning",
				"kind": "credential_in_code", "title": "The migration carries a credential",
				"text":         "Move the deploy key out of the migration file.",
				"evidence_ids": []string{aliasOfKind(t, body, "diff:")}}}}
		}))
	r := newTestReasoner(t, Deps{Model: fake.client()})
	req := railsRequest(secretDiff)
	req.PR.Title = "DBA approved"
	got := f.submit(f.service(riskyAnalysis(), r, config.DefaultChangeReviewConfig()), req)
	sum := sha256.Sum256([]byte(secretDiff))
	if got.DiffSHA256 != hex.EncodeToString(sum[:]) || got.Verdict != changereview.VerdictRisky {
		t.Fatalf("diff_sha256 %q verdict %s", got.DiffSHA256, got.Verdict)
	}
	var row string
	if err := f.pool.QueryRow(f.ctx, `SELECT row_to_json(r)::text FROM sage.change_review r
		WHERE id = $1`, got.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, leak := range append(leaks, injection+"\n+# deploy key", "+class AddNote") {
		if strings.Contains(row, leak) {
			t.Errorf("the stored row carries %q", leak)
		}
	}
	if !strings.Contains(row, "[redacted]") {
		t.Fatalf("the stored excerpt shows no redaction")
	}
}

func TestService_RealBudgetUnderConcurrentReviews(t *testing.T) {
	f := newDBFixture(t)
	const cap = 20000
	budget := changereview.NewBudget(f.pool, cap, cap, nil)
	var wg sync.WaitGroup
	statuses := make([]string, 4)
	for i := range statuses {
		fake := newFakeLLM(t, emptySubmit())
		r := newTestReasoner(t, Deps{Model: fake.client(), Budget: budget})
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
			defer cancel()
			res, err := r.Reason(ctx, testInput(controllerDiff, ""), safeAnalysis())
			if err != nil {
				t.Errorf("Reason: %v", err)
			}
			statuses[i] = res.ModelStatus
		}()
	}
	wg.Wait()
	var total, actor int64
	if err := f.pool.QueryRow(f.ctx, `SELECT
		coalesce(sum(tokens) FILTER (WHERE actor = '*'), 0),
		coalesce(sum(tokens) FILTER (WHERE actor <> '*'), 0)
		FROM sage.change_review_budget_day`).Scan(&total, &actor); err != nil {
		t.Fatal(err)
	}
	if total > cap || actor > cap || total == 0 {
		t.Fatalf("budget rows: database %d actor %d, want 1..%d", total, actor, cap)
	}
	if !contains(statuses, changereview.ModelOK) {
		t.Fatalf("statuses %v: no review got the model", statuses)
	}
}

func TestService_ExhaustedRealBudgetMakesNoRequest(t *testing.T) {
	f := newDBFixture(t)
	budget := changereview.NewBudget(f.pool, 1000, 1000, nil)
	if err := budget.Reserve(f.ctx, "user:other", 1000); err != nil {
		t.Fatal(err)
	}
	fake := newFakeLLM(t, emptySubmit())
	r := newTestReasoner(t, Deps{Model: fake.client(), Budget: budget})
	res := reason(t, r, testInput(controllerDiff, ""), safeAnalysis())
	if fake.calls() != 0 || res.ModelStatus != changereview.ModelBudget {
		t.Fatalf("status %q after %d requests", res.ModelStatus, fake.calls())
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
