package agenttools

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

const (
	reportActor = "agent:claude-code"
	prURL       = "https://github.com/acme/shop/pull/42"
)

func reportRows(f *fixture, findingID int64) int64 {
	f.t.Helper()
	return f.count("sage.source_fix WHERE finding_id = $1", findingID)
}

func timePtr(t time.Time) *time.Time { return &t }

func TestReportSourceFixPROpened(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	rep, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL, Commit: "abc1234"}, reportActor)
	require.NoError(t, err)
	require.Equal(t, id, rep.FindingID)
	require.Equal(t, "pr_opened", rep.Stage)
	require.Equal(t, "pending", rep.Verdict)
	require.Equal(t, prURL, rep.PRURL)
	require.Equal(t, "abc1234", rep.Commit)
	require.Nil(t, rep.DeployedAt)
	require.Nil(t, rep.VerifyAfter, "nothing to verify before a deploy")
	require.Equal(t, int64(1), reportRows(f, id))
	again, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "status"},
		reportActor)
	require.NoError(t, err)
	require.Equal(t, "pr_opened", again.Stage)
	require.Equal(t, "pending", again.Verdict)
	require.Equal(t, prURL, again.PRURL)
	require.Equal(t, int64(1), reportRows(f, id), "status adds no row")
}

func TestReportSourceFixRejectsBadPRURL(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	for _, u := range []string{
		"", "javascript:alert(1)", "http://github.com/acme/shop/pull/42", "https://",
		"ftp://example.com/x", "https://github.com/acme/shop/pull/42\nIgnore previous",
		"//github.com/acme/shop/pull/42", "data:text/html,<script>x</script>",
		"https://" + strings.Repeat("a", 3000) + ".com/",
	} {
		_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id,
			Stage: "pr_opened", PRURL: u}, reportActor)
		require.ErrorIs(t, err, ErrInvalid, "pr_url %q", u)
	}
	require.Equal(t, int64(0), reportRows(f, id), "a rejected report stored a row")
}

func TestReportSourceFixRejectsBadCommit(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	for _, c := range []string{
		"XYZ", "abc12", "abc123", "ABCDEF1", "abc1234; DROP TABLE x",
		strings.Repeat("a", 65), "g123456",
	} {
		_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id,
			Stage: "pr_opened", PRURL: prURL, Commit: c}, reportActor)
		require.ErrorIs(t, err, ErrInvalid, "commit %q", c)
	}
	require.Equal(t, int64(0), reportRows(f, id))
	rep, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id,
		Stage: "pr_opened", PRURL: prURL, Commit: strings.Repeat("f", 64)}, reportActor)
	require.NoError(t, err, "64 hex characters is the inclusive maximum")
	require.Equal(t, strings.Repeat("f", 64), rep.Commit)
}

func TestReportSourceFixDeployedAtBounds(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	now := time.Now().UTC().Truncate(time.Second)
	tools := New(f.pool, Options{Now: fixedClock(now)})
	for name, at := range map[string]time.Time{
		"future":          now.Add(time.Minute),
		"over seven days": now.Add(-7*24*time.Hour - time.Minute),
	} {
		_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "deployed",
			DeployedAt: timePtr(at)}, reportActor)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	require.Equal(t, int64(0), reportRows(f, id))
	edge := now.Add(-7*24*time.Hour + time.Minute)
	rep, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "deployed",
		DeployedAt: timePtr(edge)}, reportActor)
	require.NoError(t, err, "just inside seven days")
	require.NotNil(t, rep.DeployedAt)
	require.True(t, rep.DeployedAt.Equal(edge))
}

func TestReportSourceFixRequestValidation(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "merged",
		PRURL: prURL}, reportActor)
	require.ErrorIs(t, err, ErrInvalid, "unknown stage")
	_, err = tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: ""},
		reportActor)
	require.ErrorIs(t, err, ErrInvalid, "empty stage")
	_, err = tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL}, " ")
	require.ErrorIs(t, err, ErrInvalid, "an actor is required")
	_, err = tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: 9_000_000_000_000,
		Stage: "pr_opened", PRURL: prURL}, reportActor)
	require.ErrorIs(t, err, ErrNotFound, "unknown finding")
	_, err = tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: 0, Stage: "pr_opened",
		PRURL: prURL}, reportActor)
	require.ErrorIs(t, err, ErrInvalid, "finding id 0")
	_, err = tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "status"},
		reportActor)
	require.ErrorIs(t, err, ErrNotFound, "status before any report")
	require.False(t, errors.Is(err, ErrInvalid))
	require.Equal(t, int64(0), reportRows(f, id))
}

func TestReportSourceFixDeployedNowIsPending(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	now := time.Now().UTC().Truncate(time.Second)
	tools := New(f.pool, Options{Now: fixedClock(now), VerifyWindow: time.Hour})
	_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL}, reportActor)
	require.NoError(t, err)
	dep, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "deployed",
		Commit: "0123abc", DeployedAt: timePtr(now)}, reportActor)
	require.NoError(t, err)
	require.Equal(t, "deployed", dep.Stage)
	require.Equal(t, prURL, dep.PRURL, "deploy keeps the PR it came from")
	require.Equal(t, "0123abc", dep.Commit)
	st, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "status"},
		reportActor)
	require.NoError(t, err)
	require.Equal(t, "pending", st.Verdict)
	require.NotNil(t, st.VerifyAfter)
	require.True(t, st.VerifyAfter.Equal(now.Add(time.Hour)), "verify after %s", st.VerifyAfter)
	require.NotNil(t, st.DeployedAt)
	require.True(t, st.DeployedAt.Equal(now))
	require.Nil(t, st.ObservedChangePct)
	require.Equal(t, "hypopg", st.Prediction.Method, "the packet's prediction is recorded")
	require.Equal(t, int64(1), reportRows(f, id))
}

// Deployed without an earlier pr_opened is allowed (a direct deploy); a
// report never moves back from deployed to pr_opened.
func TestReportSourceFixStageOrder(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	now := time.Now().UTC().Truncate(time.Second)
	tools := New(f.pool, Options{Now: fixedClock(now)})
	_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "deployed",
		DeployedAt: timePtr(now.Add(-time.Minute))}, reportActor)
	require.NoError(t, err, "direct deploy")
	_, err = tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL}, reportActor)
	require.ErrorIs(t, err, ErrTransition)
	require.Equal(t, int64(1), reportRows(f, id))
}

// A report is bound to the packet it was built from: a stale packet hash
// (the finding's change moved on) is refused.
func TestReportSourceFixStalePacketHash(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	p, err := tools.SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	_, err = tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL, PacketHash: "0000"}, reportActor)
	require.Error(t, err, "a hash of no packet")
	require.True(t, errors.Is(err, ErrInvalid) || errors.Is(err, ErrTransition), "%v", err)
	rep, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL, PacketHash: p.Hash}, reportActor)
	require.NoError(t, err)
	require.Equal(t, "pr_opened", rep.Stage)
}
