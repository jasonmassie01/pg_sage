package agenttools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// mcp-06: a deploy's verdict is decided in the background once its verify
// window closes (DecideDue), with no agent asking; the conditional update
// stays the only writer, so a decided verdict is never rewritten. A deploy
// reported after its whole after-window had closed is flagged backdated.

// storedVerdict is the row's stage and verdict ("" while undecided) and
// whether decided_at is set.
func storedVerdict(f *fixture, findingID int64) (string, string, bool) {
	f.t.Helper()
	var stage string
	var verdict *string
	var decided bool
	err := f.pool.QueryRow(f.ctx, `SELECT stage, verdict, decided_at IS NOT NULL
		FROM sage.source_fix WHERE finding_id = $1`, findingID).Scan(&stage, &verdict, &decided)
	if err != nil {
		f.t.Fatalf("read source_fix of finding %d: %v", findingID, err)
	}
	if verdict == nil {
		return stage, "", decided
	}
	return stage, *verdict, decided
}

// deployAt reports finding id deployed at at through tools.
func deployAt(t *testing.T, f *fixture, tools *Tools, id int64, at time.Time) Report {
	t.Helper()
	rep, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "deployed",
		DeployedAt: timePtr(at)}, reportActor)
	require.NoError(t, err)
	return rep
}

// otherIndexFinding is indexFinding's recommendation under a distinct
// object identifier, so several open findings share the fixture's table
// (sage.findings dedups open findings by object).
func otherIndexFinding(t *testing.T, f *fixture, qid int64, n int) int64 {
	t.Helper()
	table := f.q("t")
	ddl := "CREATE INDEX CONCURRENTLY idx_x ON " + table + " (a)"
	return insertFinding(t, f, findingSpec{
		Category: "missing_index", Object: fmt.Sprintf("%s|btree(a)#%d", table, n),
		Title: "Index recommendation for " + table, Recommendation: "Create the index.",
		Detail: map[string]any{"table": table, "queryids": []int64{qid},
			"estimated_improvement_pct": 40, "hypopg_validated": true,
			"what_if_verdict": "verified", "mean_exec_time": 12.5, "ddl": ddl},
		SQL: strPtr(ddl), Rollback: strPtr("DROP INDEX CONCURRENTLY " + f.q("idx_x")),
	})
}

// logLines records Options.Log calls.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) log(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// State transition: a deploy whose window closed is decided with no
// status call; one inside its window stays pending; a decided row is no
// longer due.
func TestDecideDueDecidesWithoutAnAgent(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	c.deploy(t)
	fresh := otherIndexFinding(t, c.f, randomQueryID(t), 1)
	now := c.deployed.Add(3 * time.Hour)
	deployAt(t, c.f, c.tools, fresh, now.Add(-30*time.Minute))

	n, err := c.tools.DecideDue(c.f.ctx, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the closed window is due")
	stage, verdict, decided := storedVerdict(c.f, c.id)
	require.Equal(t, "verified", stage, "CHECK-04: verified with no MCP call")
	require.Equal(t, "improved", verdict)
	require.True(t, decided)
	stage, verdict, decided = storedVerdict(c.f, fresh)
	require.Equal(t, "deployed", stage, "a deploy inside its window stays undecided")
	require.Equal(t, "", verdict)
	require.False(t, decided)

	rep, err := c.tools.SourceFixReport(c.f.ctx, c.id)
	require.NoError(t, err)
	require.Equal(t, "improved", rep.Verdict)
	require.NotNil(t, rep.ObservedChangePct)
	require.InDelta(t, -80.0, *rep.ObservedChangePct, 5.0)
	again, err := c.tools.DecideDue(c.f.ctx, 50)
	require.NoError(t, err)
	require.Equal(t, 0, again, "a decided row is not due again")
}

// Boundary: deployed exactly VerifyWindow ago is due; one second later
// than that it is not.
func TestDecideDueWindowBoundary(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE INDEX idx_x ON " + sourceTable(f) + " (a)")
	now := time.Now().UTC().Truncate(time.Second)
	tools := New(f.pool, Options{Now: fixedClock(now), VerifyWindow: time.Hour})
	dueQID := randomQueryID(t)
	due := indexFinding(t, f, dueQID)
	insertSamples(t, f, dueQID, now.Add(-time.Hour), 10, 2)
	deployAt(t, f, tools, due, now.Add(-time.Hour))
	early := otherIndexFinding(t, f, randomQueryID(t), 1)
	deployAt(t, f, tools, early, now.Add(-time.Hour+time.Second))

	n, err := tools.DecideDue(f.ctx, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	stage, verdict, _ := storedVerdict(f, due)
	require.Equal(t, "verified", stage, "deployed exactly one window ago is due")
	require.Equal(t, "improved", verdict)
	stage, verdict, decided := storedVerdict(f, early)
	require.Equal(t, "deployed", stage, "one second short of the window is not due")
	require.Equal(t, "", verdict)
	require.False(t, decided)
}

// The limit bounds one pass; the oldest deploys go first.
func TestDecideDueLimitTakesTheOldestFirst(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	now := time.Now().UTC().Truncate(time.Second)
	tools := New(f.pool, Options{Now: fixedClock(now), VerifyWindow: time.Hour})
	older := indexFinding(t, f, randomQueryID(t))
	newer := otherIndexFinding(t, f, randomQueryID(t), 1)
	deployAt(t, f, tools, newer, now.Add(-2*time.Hour))
	deployAt(t, f, tools, older, now.Add(-3*time.Hour))
	n, err := tools.DecideDue(f.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	stage, verdict, _ := storedVerdict(f, older)
	require.Equal(t, "verified", stage, "the oldest due deploy is decided first")
	require.Equal(t, "unverifiable", verdict, "the packet's index was never created")
	stage, _, decided := storedVerdict(f, newer)
	require.Equal(t, "deployed", stage)
	require.False(t, decided, "the second due row waits for the next pass")
	n, err = tools.DecideDue(f.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	stage, _, _ = storedVerdict(f, newer)
	require.Equal(t, "verified", stage)
}

// Error propagation: a row whose prediction cannot be decoded is logged
// and skipped; the next due row is still decided.
func TestDecideDueSkipsABrokenRow(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	now := time.Now().UTC().Truncate(time.Second)
	logs := &logLines{}
	tools := New(f.pool, Options{Now: fixedClock(now), VerifyWindow: time.Hour, Log: logs.log})
	broken := indexFinding(t, f, randomQueryID(t))
	good := otherIndexFinding(t, f, randomQueryID(t), 1)
	deployAt(t, f, tools, broken, now.Add(-4*time.Hour))
	deployAt(t, f, tools, good, now.Add(-3*time.Hour))
	f.execArgs(`UPDATE sage.source_fix SET prediction = '"not an object"'::jsonb
		WHERE finding_id = $1`, broken)

	n, err := tools.DecideDue(f.ctx, 50)
	require.NoError(t, err, "one row's failure does not fail the pass")
	require.Equal(t, 1, n)
	stage, _, decided := storedVerdict(f, broken)
	require.Equal(t, "deployed", stage)
	require.False(t, decided, "a broken row is never given a verdict")
	stage, verdict, decided := storedVerdict(f, good)
	require.Equal(t, "verified", stage, "the row after the broken one is still decided")
	require.Equal(t, "unverifiable", verdict)
	require.True(t, decided)
	logged := logs.all()
	require.Contains(t, logged, fmt.Sprintf("finding %d", broken), "the log names the row")
	require.Contains(t, logged, "prediction", "the log says what could not be read")
	require.NotContains(t, logged, fmt.Sprintf("finding %d:", good))
}

// Nil/empty: no due rows decides nothing and is not an error.
func TestDecideDueNoRows(t *testing.T) {
	f := newFixture(t)
	pool := extraDatabase(t, f.ctx, "due_empty")
	tools := New(pool, Options{VerifyWindow: time.Hour})
	n, err := tools.DecideDue(f.ctx, 50)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

// Invalid input and a missing pool fail with distinguishable errors.
func TestDecideDueRejectsBadCalls(t *testing.T) {
	n, err := New(nil, Options{}).DecideDue(context.Background(), 50)
	require.ErrorIs(t, err, ErrUnavailable)
	require.Equal(t, 0, n)
	var nilTools *Tools
	_, err = nilTools.DecideDue(context.Background(), 50)
	require.ErrorIs(t, err, ErrUnavailable)
	f := newFixture(t)
	for _, limit := range []int{0, -1} {
		n, err := New(f.pool, Options{}).DecideDue(f.ctx, limit)
		require.ErrorIs(t, err, ErrInvalid, "limit %d", limit)
		require.Equal(t, 0, n)
	}
}

// A cancelled context stops the pass with the context's error.
func TestDecideDueCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err := New(f.pool, Options{}).DecideDue(ctx, 50)
	require.ErrorIs(t, err, context.Canceled)
}

// Concurrency (CHECK-05): DecideDue and an agent's status call race on
// one due row, 50 times. Each time exactly one verdict is stored and both
// callers see it.
func TestDecideDueRacesAnAgentStatusCall(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	for i := 0; i < 50; i++ {
		c.f.execArgs("DELETE FROM sage.source_fix WHERE finding_id = $1", c.id)
		c.deploy(t)
		var wg sync.WaitGroup
		var n int
		var dueErr, agentErr error
		var agent Report
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			n, dueErr = c.tools.DecideDue(c.f.ctx, 50)
		}()
		go func() {
			defer wg.Done()
			<-start
			agent, agentErr = c.tools.ReportSourceFix(c.f.ctx, ReportRequest{
				FindingID: c.id, Stage: "status"}, reportActor)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, dueErr, "iteration %d", i)
		require.NoError(t, agentErr, "iteration %d", i)
		require.True(t, n == 0 || n == 1, "iteration %d: decided %d", i, n)
		require.Equal(t, "improved", agent.Verdict, "iteration %d", i)
		stored, err := c.tools.SourceFixReport(c.f.ctx, c.id)
		require.NoError(t, err)
		require.Equal(t, agent.Verdict, stored.Verdict, "iteration %d", i)
		require.NotNil(t, stored.ObservedChangePct)
		require.NotNil(t, agent.ObservedChangePct)
		require.Equal(t, *agent.ObservedChangePct, *stored.ObservedChangePct,
			"iteration %d: both callers return the stored verdict", i)
		require.Equal(t, int64(1), c.f.count(`sage.source_fix WHERE finding_id = $1
			AND decided_at IS NOT NULL`, c.id), "iteration %d", i)
	}
}

// SourceFixReport reads the stored report as is: a due report it reads
// stays pending, and a finding without one is ErrNotFound.
func TestSourceFixReportReadsWithoutDeciding(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	_, err := c.tools.SourceFixReport(c.f.ctx, c.id)
	require.ErrorIs(t, err, ErrNotFound)
	c.deploy(t)
	rep, err := c.tools.SourceFixReport(c.f.ctx, c.id)
	require.NoError(t, err)
	require.Equal(t, "pending", rep.Verdict)
	require.Equal(t, "deployed", rep.Stage)
	require.NotNil(t, rep.DeployedAt)
	require.True(t, rep.DeployedAt.Equal(c.deployed))
	stage, _, decided := storedVerdict(c.f, c.id)
	require.Equal(t, "deployed", stage, "a read never decides")
	require.False(t, decided)
	_, err = c.tools.SourceFixReport(c.f.ctx, 0)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = New(nil, Options{}).SourceFixReport(c.f.ctx, c.id)
	require.ErrorIs(t, err, ErrUnavailable)
}

// Boundary: with a 2 h window, a deploy reported 3 h (or 2 h and 1 s)
// after deployed_at is backdated; one reported exactly 2 h after is not.
func TestReportBackdatedBoundary(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	now := time.Now().UTC().Truncate(time.Second)
	tools := New(f.pool, Options{Now: fixedClock(now), VerifyWindow: 2 * time.Hour})
	id := indexFinding(t, f, randomQueryID(t))
	cases := []struct {
		ago  time.Duration
		want bool
	}{
		{2 * time.Hour, false},
		{2*time.Hour + time.Second, true},
		{3 * time.Hour, true},
		{time.Minute, false},
	}
	for _, tc := range cases {
		rep := deployAt(t, f, tools, id, now.Add(-tc.ago))
		require.Equal(t, tc.want, rep.Backdated, "reported %s after the deploy", tc.ago)
		require.NotNil(t, rep.DeployReportedAt, "reported %s after", tc.ago)
		require.True(t, rep.DeployReportedAt.Equal(now), "deploy_reported_at %s, want %s",
			rep.DeployReportedAt, now)
		read, err := tools.SourceFixReport(f.ctx, id)
		require.NoError(t, err)
		require.Equal(t, tc.want, read.Backdated, "stored report, %s after", tc.ago)
	}
}

// Rows reported before deploy_reported_at existed are not backdated: the
// time is unknown.
func TestReportWithoutDeployReportedAtIsNotBackdated(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	now := time.Now().UTC().Truncate(time.Second)
	tools := New(f.pool, Options{Now: fixedClock(now), VerifyWindow: time.Hour})
	id := indexFinding(t, f, randomQueryID(t))
	deployAt(t, f, tools, id, now.Add(-6*time.Hour))
	f.execArgs("UPDATE sage.source_fix SET deploy_reported_at = NULL WHERE finding_id = $1",
		id)
	rep, err := tools.SourceFixReport(f.ctx, id)
	require.NoError(t, err)
	require.False(t, rep.Backdated)
	require.Nil(t, rep.DeployReportedAt)
}

// A pull request report carries no deploy time and is never backdated.
func TestPROpenedReportIsNotBackdated(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	tools := New(f.pool, Options{VerifyWindow: time.Hour})
	id := indexFinding(t, f, randomQueryID(t))
	rep, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL}, reportActor)
	require.NoError(t, err)
	require.False(t, rep.Backdated)
	require.Nil(t, rep.DeployReportedAt)
	require.Equal(t, prURL, rep.PRURL)
}

// State transition: re-reporting the same deploy after its window closed
// keeps the time pg_sage first heard of it, so a live deploy is not turned
// into a backdated one; a deploy with a new deployed_at is a new report.
func TestReReportedDeployKeepsItsFirstReportTime(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	first := time.Now().UTC().Truncate(time.Second)
	now := first
	tools := New(f.pool, Options{Now: func() time.Time { return now },
		VerifyWindow: 2 * time.Hour})
	id := indexFinding(t, f, randomQueryID(t))
	deployed := first.Add(-time.Minute)
	deployAt(t, f, tools, id, deployed)

	now = first.Add(3 * time.Hour)
	rep := deployAt(t, f, tools, id, deployed)
	require.NotNil(t, rep.DeployReportedAt)
	require.True(t, rep.DeployReportedAt.Equal(first), "deploy_reported_at %s, want %s",
		rep.DeployReportedAt, first)
	require.False(t, rep.Backdated, "a live deploy re-reported late is not backdated")

	moved := now.Add(-time.Minute)
	rep = deployAt(t, f, tools, id, moved)
	require.NotNil(t, rep.DeployReportedAt)
	require.True(t, rep.DeployReportedAt.Equal(now), "a new deploy time is a new report")
	require.True(t, rep.DeployedAt.Equal(moved))
	require.False(t, rep.Backdated)

	f.execArgs("UPDATE sage.source_fix SET deploy_reported_at = NULL WHERE finding_id = $1",
		id)
	rep = deployAt(t, f, tools, id, moved)
	require.Nil(t, rep.DeployReportedAt, "an unknown report time stays unknown")
	require.False(t, rep.Backdated)
}
