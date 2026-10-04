package agenttools

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// verdictCase is a finding deployed three hours before a pinned clock,
// with a one-hour verify window, so its report is due.
type verdictCase struct {
	f        *fixture
	id, qid  int64
	deployed time.Time
	tools    *Tools
}

func newVerdictCase(t *testing.T, createIndex bool) verdictCase {
	t.Helper()
	f := newFixture(t)
	table := sourceTable(f)
	if createIndex {
		f.exec("CREATE INDEX idx_x ON " + table + " (a)")
	}
	qid := randomQueryID(t)
	now := time.Now().UTC().Truncate(time.Second)
	c := verdictCase{f: f, id: indexFinding(t, f, qid), qid: qid,
		deployed: now.Add(-3 * time.Hour)}
	c.tools = New(f.pool, Options{Now: fixedClock(now), VerifyWindow: time.Hour})
	return c
}

func (c verdictCase) deploy(t *testing.T) {
	t.Helper()
	_, err := c.tools.ReportSourceFix(c.f.ctx, ReportRequest{FindingID: c.id,
		Stage: "deployed", DeployedAt: timePtr(c.deployed)}, reportActor)
	require.NoError(t, err)
}

func (c verdictCase) status(t *testing.T) Report {
	t.Helper()
	rep, err := c.tools.ReportSourceFix(c.f.ctx, ReportRequest{FindingID: c.id,
		Stage: "status"}, reportActor)
	require.NoError(t, err)
	return rep
}

func TestReportSourceFixImproved(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	c.deploy(t)
	rep := c.status(t)
	require.Equal(t, "improved", rep.Verdict, rep.Reason)
	require.Equal(t, "met", rep.Tolerance, "-80% meets a -40% prediction")
	require.NotNil(t, rep.ObservedChangePct)
	require.Less(t, *rep.ObservedChangePct, 0.0)
	require.InDelta(t, -80.0, *rep.ObservedChangePct, 5.0)
	require.Equal(t, "hypopg", rep.Prediction.Method)
	require.Equal(t, []int64{c.qid}, rep.Prediction.TargetQueryIDs)
	require.NotEmpty(t, rep.Evidence, "the verdict carries its measurements")
	require.NotNil(t, rep.VerifyAfter)
	require.True(t, rep.VerifyAfter.Equal(c.deployed.Add(time.Hour)))
	require.Equal(t, int64(1), reportRows(c.f, c.id))
}

func TestReportSourceFixRegressed(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 2, 10)
	c.deploy(t)
	rep := c.status(t)
	require.Equal(t, "regressed", rep.Verdict, rep.Reason)
	require.NotNil(t, rep.ObservedChangePct)
	require.Greater(t, *rep.ObservedChangePct, 0.0)
	require.Equal(t, "missed", rep.Tolerance)
	require.NotEmpty(t, rep.Reason)
}

func TestReportSourceFixNoSamplesIsInsufficient(t *testing.T) {
	c := newVerdictCase(t, true)
	c.deploy(t)
	rep := c.status(t)
	require.Equal(t, "insufficient_evidence", rep.Verdict)
	require.Equal(t, "unmeasured", rep.Tolerance)
	require.Nil(t, rep.ObservedChangePct)
	require.NotEmpty(t, rep.Reason)
}

// A packet whose index never appeared in the catalog was not deployed as
// described: the change cannot be verified, however the query moved.
func TestReportSourceFixMissingIndexIsUnverifiable(t *testing.T) {
	c := newVerdictCase(t, false)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	c.deploy(t)
	rep := c.status(t)
	require.Equal(t, "unverifiable", rep.Verdict)
	require.Contains(t, rep.Reason, "idx_x", "reason names the missing index")
	require.Equal(t, "unmeasured", rep.Tolerance)
}

func TestReportSourceFixDecidedVerdictIsImmutable(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	c.deploy(t)
	first := c.status(t)
	require.Equal(t, "improved", first.Verdict)
	require.NotNil(t, first.ObservedChangePct)
	deleteSamples(t, c.f, c.qid)
	insertSamples(t, c.f, c.qid, c.deployed, 2, 10)
	again := c.status(t)
	require.Equal(t, "improved", again.Verdict, "a decided verdict never changes")
	require.NotNil(t, again.ObservedChangePct)
	require.Equal(t, *first.ObservedChangePct, *again.ObservedChangePct)
	fresh := New(c.f.pool, Options{Now: fixedClock(c.deployed.Add(5 * time.Hour)),
		VerifyWindow: time.Hour})
	stored, err := fresh.ReportSourceFix(c.f.ctx, ReportRequest{FindingID: c.id,
		Stage: "status"}, reportActor)
	require.NoError(t, err)
	require.Equal(t, "improved", stored.Verdict, "the verdict is stored, not cached")
}

func TestReportSourceFixNoReportsAfterVerdict(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	c.deploy(t)
	require.Equal(t, "improved", c.status(t).Verdict)
	_, err := c.tools.ReportSourceFix(c.f.ctx, ReportRequest{FindingID: c.id,
		Stage: "deployed", DeployedAt: timePtr(c.deployed.Add(time.Hour))}, reportActor)
	require.ErrorIs(t, err, ErrTransition, "deployed after verified")
	_, err = c.tools.ReportSourceFix(c.f.ctx, ReportRequest{FindingID: c.id,
		Stage: "pr_opened", PRURL: prURL}, reportActor)
	require.ErrorIs(t, err, ErrTransition, "pr_opened after verified")
	rep := c.status(t)
	require.Equal(t, "improved", rep.Verdict)
	require.NotNil(t, rep.DeployedAt)
	require.True(t, rep.DeployedAt.Equal(c.deployed), "a refused report changed the row")
}

// Before the window closes the report stays pending even with evidence.
func TestReportSourceFixNotDueStaysPending(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	early := New(c.f.pool, Options{Now: fixedClock(c.deployed.Add(30 * time.Minute)),
		VerifyWindow: time.Hour})
	_, err := early.ReportSourceFix(c.f.ctx, ReportRequest{FindingID: c.id,
		Stage: "deployed", DeployedAt: timePtr(c.deployed)}, reportActor)
	require.NoError(t, err)
	rep, err := early.ReportSourceFix(c.f.ctx, ReportRequest{FindingID: c.id,
		Stage: "status"}, reportActor)
	require.NoError(t, err)
	require.Equal(t, "pending", rep.Verdict)
	require.Nil(t, rep.ObservedChangePct)
	require.Equal(t, "improved", c.status(t).Verdict, "decided once due")
}

// Concurrent status calls on a due report decide it once: one row, one
// verdict, every caller sees it.
func TestReportSourceFixConcurrentStatusDecidesOnce(t *testing.T) {
	c := newVerdictCase(t, true)
	insertSamples(t, c.f, c.qid, c.deployed, 10, 2)
	c.deploy(t)
	const n = 8
	var wg sync.WaitGroup
	reports := make([]Report, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			reports[i], errs[i] = c.tools.ReportSourceFix(c.f.ctx, ReportRequest{
				FindingID: c.id, Stage: "status"}, reportActor)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "caller %d", i)
		require.Equal(t, "improved", reports[i].Verdict, "caller %d", i)
		require.NotNil(t, reports[i].ObservedChangePct, "caller %d", i)
		require.Equal(t, *reports[0].ObservedChangePct, *reports[i].ObservedChangePct)
	}
	require.Equal(t, int64(1), reportRows(c.f, c.id))
}
