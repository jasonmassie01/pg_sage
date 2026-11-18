package gitops

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
)

// Review cases of cli-02b on a real server: a PR the host opened before a
// later step failed is tracked, and a retry after a partial open uses a
// new branch.

// labelFailHost opens the PR, then fails adding its labels, as the GitHub
// client does when the label call is refused.
type labelFailHost struct{ *codehosttest.Fake }

func (h labelFailHost) OpenPR(ctx context.Context, repo string, req codehost.PRRequest,
) (codehost.PullRequest, error) {
	pr, err := h.Fake.OpenPR(ctx, repo, req)
	if err != nil {
		return pr, err
	}
	return pr, fmt.Errorf("pull request %s#%d opened, labels not added: %w: HTTP 502",
		repo, pr.Number, codehost.ErrUnavailable)
}

// branchOnceHost refuses a branch that an earlier attempt created, as a
// real host does, and fails the first attempt after creating its branch.
type branchOnceHost struct {
	*codehosttest.Fake
	mu        sync.Mutex
	seen      map[string]bool
	failFirst bool
}

func (h *branchOnceHost) OpenPR(ctx context.Context, repo string, req codehost.PRRequest,
) (codehost.PullRequest, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen[req.Branch] {
		return codehost.PullRequest{}, fmt.Errorf("%w: POST /git/refs: HTTP 422: "+
			"Reference already exists", codehost.ErrInvalid)
	}
	h.seen[req.Branch] = true
	if h.failFirst {
		h.failFirst = false
		return codehost.PullRequest{}, fmt.Errorf("%w: POST /pulls: HTTP 502",
			codehost.ErrUnavailable)
	}
	return h.Fake.OpenPR(ctx, repo, req)
}

func TestApprovalPR_OpenedPRWithAFailedLabelStepIsTracked(t *testing.T) {
	f := newPRFixture(t)
	f.prs.Host = labelFailHost{f.host}
	id := f.queueChange(t, "ANALYZE public.orders", "safe")
	if res := f.sync(t); res.Opened != 1 || res.Failed != 0 {
		t.Fatalf("result %+v, want the opened PR counted as opened", res)
	}
	rec := f.row(t, id)
	if rec.State != PRStateOpen || rec.PRNumber != 1 || rec.PRURL == "" {
		t.Fatalf("row = %+v, want open PR #1", rec)
	}
	if f.prs.Disabled() != "" {
		t.Fatalf("a label failure turned PR mode off: %q", f.prs.Disabled())
	}
	f.now = f.now.Add(2 * time.Hour)
	f.sync(t)
	if n := len(f.host.Opened()); n != 1 {
		t.Fatalf("%d PRs opened for one change, want 1", n)
	}
	f.host.Merge(prRepo, 1, "octo-op", nil)
	f.sync(t)
	if runs, _ := f.queue.executions(); runs != 1 {
		t.Fatalf("the merge of the tracked PR ran the change %d times, want 1", runs)
	}
}

func TestApprovalPR_RetryAfterAPartialOpenUsesANewBranch(t *testing.T) {
	f := newPRFixture(t)
	host := &branchOnceHost{Fake: f.host, seen: map[string]bool{}, failFirst: true}
	f.prs.Host = host
	id := f.queueChange(t, "ANALYZE public.orders", "safe")
	f.sync(t)
	first := f.row(t, id)
	if first.State != PRStateOpenFailed || first.Detail != "PR not opened: HTTP 502" {
		t.Fatalf("after the partial open = %+v, want open_failed, HTTP 502", first)
	}
	f.now = first.NextAttemptAt.Add(time.Second)
	f.sync(t)
	rec := f.row(t, id)
	if rec.State != PRStateOpen || rec.PRNumber != 1 {
		t.Fatalf("retry = %+v, want open PR #1 (the first branch already exists)", rec)
	}
	opened := f.host.Opened()
	want := fmt.Sprintf("pg_sage/approval-%d-2", rec.ID)
	if len(opened) != 1 || opened[0].Branch != want {
		t.Fatalf("opened %+v, want one PR on branch %s", opened, want)
	}
}
