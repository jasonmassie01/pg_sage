package agenttools

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/codehost"
)

// PRRecorder is PR mode's cases.SourceFixRecorder over Tools: a PR is
// recorded only for a finding with a source-fix packet.
type PRRecorder struct{ Tools *Tools }

var _ cases.SourceFixRecorder = PRRecorder{}

// CanRecord says why the finding's PR could not be recorded: no such
// finding, or no source change in it. Other failures are errors.
func (r PRRecorder) CanRecord(ctx context.Context, findingID int64) (string, error) {
	_, err := r.Tools.SourceFixPacket(ctx, findingID)
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoChange):
		return "no source-fix packet: " + err.Error(), nil
	}
	return "", err
}

// RecordOpened records pr as PR mode's pull request on the finding.
func (r PRRecorder) RecordOpened(ctx context.Context, findingID int64, repo string,
	pr codehost.PullRequest) error {
	_, err := r.Tools.RecordOpenedPR(ctx, OpenedPR{FindingID: findingID, URL: pr.URL,
		Repo: repo, Number: pr.Number})
	return err
}
