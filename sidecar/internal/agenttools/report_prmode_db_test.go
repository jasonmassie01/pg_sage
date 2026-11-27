package agenttools

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// pr-01b: PR mode records the PR it opened through the pr_opened stage,
// marked origin pg_sage with its repository, number and state.
// No concurrent test here: the upsert is one statement (report_db_test.go
// covers its conflict rule); PR mode's own lock is tested in cases.

func TestRecordOpenedPRWritesOriginRepoNumberAndState(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	rep, err := tools.RecordOpenedPR(f.ctx, OpenedPR{FindingID: id, URL: prURL,
		Repo: "acme/shop", Number: 42})
	require.NoError(t, err)
	require.Equal(t, "pr_opened", rep.Stage)
	require.Equal(t, prURL, rep.PRURL)
	require.Equal(t, "pending", rep.Verdict)
	var origin, repo, state, by string
	var number int
	f.scalar(&origin, `SELECT origin FROM sage.source_fix WHERE finding_id = $1`, id)
	f.scalar(&repo, `SELECT repo FROM sage.source_fix WHERE finding_id = $1`, id)
	f.scalar(&number, `SELECT pr_number FROM sage.source_fix WHERE finding_id = $1`, id)
	f.scalar(&state, `SELECT pr_state FROM sage.source_fix WHERE finding_id = $1`, id)
	f.scalar(&by, `SELECT reported_by FROM sage.source_fix WHERE finding_id = $1`, id)
	require.Equal(t, "pg_sage", origin)
	require.Equal(t, "acme/shop", repo)
	require.Equal(t, 42, number)
	require.Equal(t, "open", state)
	require.Equal(t, PRModeActor, by)

	// Recording again (a retry after a lost answer) keeps one row.
	_, err = tools.RecordOpenedPR(f.ctx, OpenedPR{FindingID: id, URL: prURL,
		Repo: "acme/shop", Number: 42})
	require.NoError(t, err)
	require.Equal(t, int64(1), reportRows(f, id))
}

func TestAgentReportKeepsOriginAgent(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: id, Stage: "pr_opened",
		PRURL: prURL}, reportActor)
	require.NoError(t, err)
	var origin string
	var number *int
	f.scalar(&origin, `SELECT origin FROM sage.source_fix WHERE finding_id = $1`, id)
	f.scalar(&number, `SELECT pr_number FROM sage.source_fix WHERE finding_id = $1`, id)
	require.Equal(t, "agent", origin)
	require.Nil(t, number, "an agent's report names no PR number")
}

func TestRecordOpenedPRRefusesBadInput(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	for name, pr := range map[string]OpenedPR{
		"no finding":   {URL: prURL, Repo: "acme/shop", Number: 1},
		"http url":     {FindingID: id, URL: "http://x.test/1", Repo: "acme/shop", Number: 1},
		"zero number":  {FindingID: id, URL: prURL, Repo: "acme/shop"},
		"bad repo":     {FindingID: id, URL: prURL, Repo: "../x", Number: 1},
		"missing repo": {FindingID: id, URL: prURL, Number: 1},
	} {
		_, err := tools.RecordOpenedPR(f.ctx, pr)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	_, err := tools.RecordOpenedPR(f.ctx, OpenedPR{FindingID: id + 1000, URL: prURL,
		Repo: "acme/shop", Number: 1})
	require.ErrorIs(t, err, ErrNotFound, "an unknown finding has no packet")
	require.Equal(t, int64(0), reportRows(f, id))
}
