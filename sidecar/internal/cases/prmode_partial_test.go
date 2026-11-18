package cases

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// prKeyPattern is the shape of every candidate key: safe in a branch name
// and in the hidden marker.
var prKeyPattern = regexp.MustCompile(`^f[0-9]+-[a-z_]+-[0-9a-f]{12}(-revert)?$`)

// labelFailHost opens the PR on the fake, then fails adding its labels, as
// GitHub's OpenPR does: the PR exists, the error says so.
type labelFailHost struct{ *codehostFake }

func (h labelFailHost) OpenPR(ctx context.Context, repo string, req codehost.PRRequest,
) (codehost.PullRequest, error) {
	pr, err := h.codehostFake.OpenPR(ctx, repo, req)
	if err != nil {
		return pr, err
	}
	return pr, fmt.Errorf("pull request %s#%d opened, labels not added: %w", repo,
		pr.Number, codehost.ErrUnavailable)
}

// A PR the host opened but could not label is recorded, so no later cycle
// opens a second one or reports it as never opened.
func TestPRModeOpenedButUnlabelledPRIsRecorded(t *testing.T) {
	fake, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(labelFailHost{fake}, rec, nil, nil)
	out, err := m.Run(context.Background(), migrationCandidate())
	require.ErrorIs(t, err, codehost.ErrUnavailable, "the label failure is still reported")
	require.Equal(t, PROpened, out.Status)
	require.Equal(t, 1, out.Number)
	require.Contains(t, out.Note, "opened as acme/shop#1")
	require.Contains(t, out.Note, "labels not added")
	row, ok := rec.row(41)
	require.True(t, ok, "the opened PR is recorded")
	require.Equal(t, 1, row.Number)
	again, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRCovered, again.Status)
	require.Equal(t, 1, len(fake.Opened()), "no second PR")
}

func TestPRModeRevertOpenedButUnlabelledIsRecordedOnce(t *testing.T) {
	fake, rec := migrationRepo(), newMemRecords()
	m := newTestPRMode(fake, rec, nil, nil)
	_, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	row, _ := rec.row(41)
	row.Stage, row.Verdict, row.PRState = "verified", "regressed", "merged"
	rec.setRow(41, row)
	m.deps.Host = labelFailHost{fake}
	out, err := m.Follow(context.Background(), migrationCandidate())
	require.ErrorIs(t, err, codehost.ErrUnavailable)
	require.Equal(t, PRReverting, out.Status)
	require.Contains(t, out.Note, "revert opened as acme/shop#2")
	require.Equal(t, []string{"revert_opened"}, rec.states)
	again, err := m.Follow(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRReverting, again.Status)
	require.Equal(t, 2, len(fake.Opened()), "the revert is opened once")
}

// A key another cycle holds is that cycle's to open: covered, no host call.
func TestPRModeHeldLockOpensNothing(t *testing.T) {
	host, rec := migrationRepo(), newMemRecords()
	rec.locks[migrationCandidate().Key()] = true
	m := newTestPRMode(host, rec, nil, nil)
	out, err := m.Run(context.Background(), migrationCandidate())
	require.NoError(t, err)
	require.Equal(t, PRCovered, out.Status)
	require.Equal(t, "another cycle is opening this PR", out.Note)
	require.Equal(t, 0, len(host.Opened()))
	require.Equal(t, 0, rec.recordCalls)
}
