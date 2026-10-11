package firstlook

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A step that outlives its client-side budget can fail with a secondary
// error (pgx closes the connection when the deadline cancels a query, and
// the next statement reports "conn closed"). The check must still be
// reported as the timeout it was, and offered for the retry.
func TestRun_StepPastItsBudgetIsReportedAsTimeout(t *testing.T) {
	pool, ctx := livePool(t)
	opts := testOptions("app")
	opts.StatementTimeout = 10 * time.Millisecond
	p := &pass{pool: pool, opts: opts.withDefaults(), report: &Report{}}
	defer p.close()
	st := step{rules: []string{"slow_rule"}, section: SectionAgentPosture,
		fn: func(sctx context.Context, _ pgx.Tx) ([]outcome, error) {
			<-sctx.Done()
			return nil, errors.New("failed to deallocate cached statement(s): conn closed")
		}}
	if err := p.run(ctx, st); err != nil {
		t.Fatalf("run: %v", err)
	}
	c := p.report.Checks[0]
	if c.Status != CheckDegraded || !strings.Contains(c.Note, "did not finish") ||
		!strings.Contains(c.Note, "timeout") || c.Section != SectionAgentPosture {
		t.Fatalf("check = %+v, want degraded by the step budget", c)
	}
	if !slices.Contains(p.report.Retryable, "slow_rule") {
		t.Fatalf("retryable = %v, want slow_rule", p.report.Retryable)
	}
}
