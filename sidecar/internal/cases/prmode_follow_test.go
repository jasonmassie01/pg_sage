package cases

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// State transitions: opened, then closed unmerged (declined) or merged and
// deployed (the source-fix report), then the verdict; a regressed
// migration with a rollback gets a revert PR, once.

func openedPR(t *testing.T) (*PRMode, *memRecords, *codehostFake) {
	t.Helper()
	host, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(host, rec, nil, nil)
	out, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PROpened, out.Status)
	return m, rec, host
}

func TestPRModeFollowOpenPRSaysOpen(t *testing.T) {
	m, rec, _ := openedPR(t)
	out, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PROpened, out.Status)
	require.Equal(t, "open as acme/shop#1", out.Note)
	require.Empty(t, rec.states, "no state change while open")
}

func TestPRModeFollowClosedUnmergedIsDeclined(t *testing.T) {
	m, rec, host := openedPR(t)
	host.SetState(prRepo, codehost.PRStatus{Number: 1, State: codehost.PRClosed})
	out, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRDeclined, out.Status)
	require.Equal(t, "declined in acme/shop#1", out.Note)
	require.Equal(t, []string{"closed"}, rec.states)
	// A declined change is not proposed again.
	again, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRDeclined, again.Status)
	require.Equal(t, 1, len(host.Opened()))
	// Following a declined PR asks the host nothing more.
	calls := host.Calls()
	_, err = m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, calls, host.Calls())
}

func TestPRModeFollowMergedThenDeployedThenVerdict(t *testing.T) {
	m, rec, host := openedPR(t)
	host.SetState(prRepo, codehost.PRStatus{Number: 1, State: codehost.PRMerged})
	out, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRMerged, out.Status)
	require.Contains(t, out.Note, "merged in acme/shop#1")
	require.Contains(t, out.Note, "deploy")
	require.Equal(t, []string{"merged"}, rec.states)

	row, _ := rec.row(41)
	row.Stage = "deployed"
	rec.setRow(41, row)
	out, err = m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Contains(t, out.Note, "deployed; pg_sage is measuring the effect")

	for _, verdict := range []string{"improved", "neutral", "insufficient_evidence"} {
		row.Stage, row.Verdict = "verified", verdict
		rec.setRow(41, row)
		out, err = m.Follow(context.Background(), migrationCandidate())
		require.NoError(t, err)
		require.Equal(t, PRVerified, out.Status, verdict)
		require.Equal(t, "verified "+verdict+" after acme/shop#1", out.Note)
	}
	require.Equal(t, 1, len(host.Opened()), "no revert for a verdict that did not regress")
}

func TestPRModeRegressedMigrationGetsOneRevertPR(t *testing.T) {
	m, rec, host := openedPR(t)
	row, _ := rec.row(41)
	row.Stage, row.Verdict, row.PRState = "verified", "regressed", "merged"
	rec.setRow(41, row)
	out, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRReverting, out.Status)
	require.Equal(t, "regressed after acme/shop#1; revert opened as acme/shop#2", out.Note)
	opened := host.Opened()
	require.Equal(t, 2, len(opened))
	revert := opened[1]
	require.Equal(t, migrationCandidate().RollbackSQL+"\n", revert.Files[0].Content)
	require.True(t, strings.HasPrefix(revert.Files[0].Path, "db/migrate/20261117093015_"))
	require.Contains(t, revert.Body, "<!-- pg_sage:key="+migrationCandidate().Key()+
		"-revert -->")
	require.Contains(t, revert.Body, "acme/shop#1")
	require.Equal(t, []string{"revert_opened"}, rec.states)
	again, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRReverting, again.Status)
	require.Equal(t, 2, len(host.Opened()), "the revert is opened once")
}

func TestPRModeRegressedWithoutRollbackOpensNoRevert(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(host, rec, nil, nil)
	c := migrationCandidate()
	c.RollbackSQL = ""
	_, err := m.Run(context.Background(), c)
	require.NoError(t, err)
	row, _ := rec.row(41)
	row.Stage, row.Verdict = "verified", "regressed"
	rec.setRow(41, row)
	out, err := m.Follow(context.Background(), c)
	require.NoError(t, err)
	require.Equal(t, PRVerified, out.Status)
	require.Contains(t, out.Note, "no rollback to revert with")
	require.Equal(t, 1, len(host.Opened()))
}

func TestPRModeRegressedRewriteOpensNoRevert(t *testing.T) {
	host, rec := rewriteRepo(), newMemRecords()
	sites := stubSites{site: PRCallSite{Path: "app/store/orders.go", Line: 3,
		Confidence: "exact"}}
	m := newTestPRMode(host, rec, nil, sites)
	_, err := m.Run(context.Background(), rewriteCandidate())
	require.NoError(t, err)
	row, _ := rec.row(52)
	row.Stage, row.Verdict = "verified", "regressed"
	rec.setRow(52, row)
	out, err := m.Follow(context.Background(), rewriteCandidate())
	require.NoError(t, err)
	require.Equal(t, PRVerified, out.Status)
	require.Contains(t, out.Note, "revert the application change")
	require.Equal(t, 1, len(host.Opened()))
}

func TestPRModeFollowIgnoresAnAgentsPR(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	rec.setRow(41, SourceFixPR{Origin: "agent", Stage: "pr_opened",
		URL: "https://github.com/acme/shop/pull/9"})
	m := newTestPRMode(host, rec, nil, nil)
	out, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PROutcome{}, out)
	run, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRCovered, run.Status)
	require.Contains(t, run.Note, "https://github.com/acme/shop/pull/9")
	require.Equal(t, 0, host.Calls(), "an agent's PR covers the finding without asking the host")
}

func TestPRModeFollowHostErrorKeepsTheState(t *testing.T) {
	m, rec, host := openedPR(t)
	host.Fail(codehost.ErrUnavailable)
	_, err := m.Follow(context.Background(), migrationCandidate())
	require.ErrorIs(t, err, codehost.ErrUnavailable)
	require.Empty(t, rec.states)
}
