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

// PRCallSites is PR mode's cases.CallSiteFinder: the call site pr-01a
// finds on Host in Repo.
type PRCallSites struct {
	Tools *Tools
	Host  codehost.Host
	Repo  string
}

var _ cases.CallSiteFinder = PRCallSites{}

// CallSite finds the statement's call site.
func (s PRCallSites) CallSite(ctx context.Context, queryID int64) (cases.PRCallSite, error) {
	site, err := s.Tools.CallSite(ctx, s.Host, s.Repo, QueryID(queryID))
	if err != nil {
		return cases.PRCallSite{}, err
	}
	return cases.PRCallSite{Path: site.Path, Line: site.Line,
		Confidence: string(site.Confidence), Note: site.Note}, nil
}
