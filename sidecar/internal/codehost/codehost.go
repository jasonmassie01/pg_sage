// Package codehost is pg_sage's client for the code host that holds an
// application's repository (pr-01a): GitHub and GitLab over their REST
// APIs. It reads files and searches code so a statement can be traced to
// the line of application code that sends it, and it lists, opens and
// comments on pull requests for PR mode (pr-01b).
//
// Every call goes through internal/egress, runs under a request timeout
// and a per-host concurrency cap, and honours the host's rate limit: after
// a 429 (or GitHub's 403 rate-limit answer) no request is sent until the
// Retry-After time has passed. The token is sent only in a request header,
// is never logged (the package logs nothing) and is redacted from every
// error.
package codehost

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Errors. Each is distinguishable with errors.Is; details are wrapped.
var (
	// ErrInvalid is a request refused before any request is sent.
	ErrInvalid = errors.New("codehost: invalid request")
	// ErrForbidden is a 401 or 403: the token cannot read or write the repo.
	ErrForbidden = errors.New("codehost: access denied")
	// ErrNotFound is a 404: no such repository, file or pull request.
	ErrNotFound = errors.New("codehost: not found")
	// ErrRateLimited is the host's rate limit; the error is a *RateLimitError.
	ErrRateLimited = errors.New("codehost: rate limited")
	// ErrUnavailable is a transport failure or a 5xx answer.
	ErrUnavailable = errors.New("codehost: unavailable")
	// ErrTooLarge is a file or answer larger than the client reads; it is
	// also ErrUnavailable. A call-site search skips such a hit.
	ErrTooLarge = fmt.Errorf("%w: too large", ErrUnavailable)
)

// RateLimitError is a rate-limit answer. RetryAfter is how long the client
// sends nothing more to the host.
type RateLimitError struct {
	Host       string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("codehost %s: rate limited, retry after %s", e.Host,
		e.RetryAfter.Round(time.Second))
}

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// Host is one code host. repo is "owner/name" (GitLab: "group/sub/name").
type Host interface {
	// Name identifies the host in messages, for example "github api.github.com".
	Name() string
	// SearchCode returns the files of repo matching query, at most
	// MaxSearchHits, on the default branch.
	SearchCode(ctx context.Context, repo, query string) ([]SearchHit, error)
	// GetFile reads path on repo's default branch.
	GetFile(ctx context.Context, repo, path string) (File, error)
	// ListOpenPRs returns the open pull requests of repo carrying label.
	ListOpenPRs(ctx context.Context, repo, label string) ([]PullRequest, error)
	// OpenPR creates req.Branch from req.Base, commits req.Files to it and
	// opens a pull request into req.Base.
	OpenPR(ctx context.Context, repo string, req PRRequest) (PullRequest, error)
	// Comment adds a comment to pull request number.
	Comment(ctx context.Context, repo string, number int, body string) error
	// PRState reads the state of pull request number.
	PRState(ctx context.Context, repo string, number int) (PRStatus, error)
}

// Bounds of one call.
const (
	// MaxSearchHits is how many files one SearchCode returns at most.
	MaxSearchHits = 20
	// MaxFileBytes is the largest file GetFile reads.
	MaxFileBytes = 1 << 20
	// DefaultTimeout bounds one request when Options.Timeout is zero.
	DefaultTimeout = 15 * time.Second
	// DefaultMaxConcurrent is the per-host cap when Options.MaxConcurrent is zero.
	DefaultMaxConcurrent = 4
)

// SearchHit is one file a code search matched.
type SearchHit struct {
	Path string `json:"path"`
}

// File is one file's content on the default branch.
type File struct {
	Path    string
	Content string
}

// PullRequest is one pull (GitLab: merge) request.
type PullRequest struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	URL    string   `json:"url"`
	Labels []string `json:"labels,omitempty"`
}

// PRRequest is a pull request to open: one new branch with one commit.
type PRRequest struct {
	Title  string
	Body   string
	Branch string
	// Base is the target branch; empty means the repository's default.
	Base          string
	CommitMessage string
	Labels        []string
	Files         []FileChange
}

// FileChange is one file written by the commit; it is created or replaced.
type FileChange struct {
	Path    string
	Content string
}

// PRStateName is a pull request's state.
type PRStateName string

// Pull request states.
const (
	PROpen   PRStateName = "open"
	PRClosed PRStateName = "closed"
	PRMerged PRStateName = "merged"
)

// PRStatus is a pull request's state and, once merged, when.
type PRStatus struct {
	Number   int         `json:"number"`
	State    PRStateName `json:"state"`
	MergedAt *time.Time  `json:"merged_at,omitempty"`
}

// Options configure a REST client; zero values mean the defaults.
type Options struct {
	// BaseURL is the API root: https://api.github.com, https://host/api/v3
	// (GitHub Enterprise) or https://gitlab.com/api/v4. Empty means the
	// public host. It must be https, except on a loopback address.
	BaseURL string
	// Token is the access token. It is sent only in a request header.
	Token string
	// Timeout bounds one request; 0 means DefaultTimeout.
	Timeout time.Duration
	// MaxConcurrent caps requests in flight to the host; 0 means 4.
	MaxConcurrent int
	// Now is the clock for rate-limit cool-downs; nil means time.Now.
	Now func() time.Time
}
