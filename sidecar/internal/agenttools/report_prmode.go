package agenttools

import (
	"context"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// Origins of a recorded PR (sage.source_fix.origin, pr-01b).
const (
	originAgent  = "agent"
	originPGSage = "pg_sage"
	// PRModeActor is reported_by on the PRs PR mode opens.
	PRModeActor = "pg_sage:pr_mode"
)

// OpenedPR is a pull request PR mode opened for a finding's source fix.
type OpenedPR struct {
	FindingID int64
	URL       string
	Repo      string
	Number    int
}

// RecordOpenedPR records PR mode's pull request through the pr_opened
// stage, as a coding agent's report would be, with origin pg_sage, its
// repository, number and state open. The deploy report and the verdict
// then reuse the source-fix verify window. Recording the same PR again
// keeps one row; a finding past pr_opened is ErrTransition.
func (t *Tools) RecordOpenedPR(ctx context.Context, pr OpenedPR) (Report, error) {
	if err := t.ready(); err != nil {
		return Report{}, err
	}
	if pr.FindingID <= 0 {
		return Report{}, invalid("finding_id %d is not positive", pr.FindingID)
	}
	if err := validPRURL(pr.URL); err != nil {
		return Report{}, err
	}
	if pr.Number <= 0 {
		return Report{}, invalid("pr number %d is not positive", pr.Number)
	}
	if err := codehost.ValidateRepo(pr.Repo); err != nil {
		return Report{}, invalid("repo %q: %v", pr.Repo, err)
	}
	req := ReportRequest{FindingID: pr.FindingID, Stage: StagePROpened, PRURL: pr.URL}
	return t.writePROpened(ctx, req, PRModeActor, prOrigin{name: originPGSage,
		repo: pr.Repo, number: pr.Number, state: "open"})
}
