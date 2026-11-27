package gitops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// cli-02b on a real server (PG14 and PG17) with the fake code host: a
// queued change is opened as one PR; a merge by a mapped operator approves
// it through the single-use, content-matched approval; anything else is
// refused, rejected, superseded or expired, each transition audited once.

func TestApprovalPR_MergeByOperatorRunsOnceAsTheMerger(t *testing.T) {
	f := newPRFixture(t)
	id := f.queueChange(t, "ANALYZE public.orders", "safe")
	res := f.sync(t)
	if res.Opened != 1 {
		t.Fatalf("opened %d PRs, want 1 (%+v)", res.Opened, res)
	}
	f.checkOpenedPR(t, id)
	rec := f.row(t, id)
	if rec.State != PRStateOpen || rec.PRNumber != 1 || rec.PRURL == "" {
		t.Fatalf("row after open = %+v, want open PR #1 with a URL", rec)
	}
	if again := f.sync(t); again.Opened != 0 || len(f.host.Opened()) != 1 {
		t.Fatalf("second sync opened %d more PRs", len(f.host.Opened())-1)
	}
	f.host.Merge(prRepo, 1, "octo-op", nil)
	f.sync(t)
	runs, by := f.queue.executions()
	if runs != 1 || by[0] != f.users["olive@example.com"] {
		t.Fatalf("executions = %d by %v, want 1 by olive", runs, by)
	}
	status, decidedBy := f.status(t, id)
	if status != "approved" || decidedBy != f.users["olive@example.com"] {
		t.Fatalf("queue item = %s by %d, want approved by olive", status, decidedBy)
	}
	rec = f.row(t, id)
	if rec.State != PRStateApproved || rec.DecidedBy != f.users["olive@example.com"] ||
		rec.DecidedAt == nil {
		t.Fatalf("row after merge = %+v, want approved by olive", rec)
	}
	f.sync(t)
	if runs, _ := f.queue.executions(); runs != 1 {
		t.Fatalf("a later sync ran the change again: %d runs", runs)
	}
	want := map[string]int{PRStateOpen: 1, PRStateApproved: 1}
	if got := f.audits(t, id); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audits = %v, want %v", got, want)
	}
}

func TestApprovalPR_MergeRefusals(t *testing.T) {
	cases := []struct {
		name, login, comment string
		edit                 func(path, content string) string
		change               bool
	}{
		{name: "viewer merger", login: "view-er", comment: "operator role"},
		{name: "unmapped merger", login: "stranger", comment: "not mapped"},
		{name: "mapped to no pg_sage user", login: "ghost", comment: "not mapped"},
		{name: "edited SQL", login: "octo-op", comment: "differs from the file",
			edit: func(_, c string) string {
				return strings.Replace(c, "ANALYZE public.orders", "DROP TABLE orders", 1)
			}},
		{name: "change edited after open", login: "octo-op", change: true,
			comment: "changed after this PR was opened"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPRFixture(t)
			id := f.queueChange(t, "ANALYZE public.orders", "safe")
			f.sync(t)
			if tc.change {
				if _, err := f.pool.Exec(context.Background(), `UPDATE sage.action_queue
					SET rollback_sql = 'SELECT 2' WHERE id = $1`, id); err != nil {
					t.Fatal(err)
				}
			}
			f.host.Merge(prRepo, 1, tc.login, tc.edit)
			f.sync(t)
			if runs, _ := f.queue.executions(); runs != 0 {
				t.Fatalf("a refused merge ran the change %d times", runs)
			}
			if status, _ := f.status(t, id); status != "pending" {
				t.Fatalf("queue item = %s, want still pending in pg_sage", status)
			}
			rec := f.row(t, id)
			if rec.State != PRStateRefused || !strings.Contains(rec.Detail, tc.comment) {
				t.Fatalf("row = %+v, want refused mentioning %q", rec, tc.comment)
			}
			comments := f.host.Comments()
			if len(comments) != 1 || !strings.Contains(comments[0], tc.comment) {
				t.Fatalf("comments = %q, want one mentioning %q", comments, tc.comment)
			}
		})
	}
}

func TestApprovalPR_ClosedUnmergedRejects(t *testing.T) {
	f := newPRFixture(t)
	id := f.queueChange(t, "ANALYZE public.orders", "safe")
	f.sync(t)
	if err := f.host.ClosePR(context.Background(), prRepo, 1); err != nil {
		t.Fatal(err)
	}
	f.sync(t)
	var status, reason string
	if err := f.pool.QueryRow(context.Background(), `SELECT status, reason
		FROM sage.action_queue WHERE id = $1`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || reason != "closed in acme/config#1" {
		t.Fatalf("queue item = %s (%q), want rejected, closed in acme/config#1", status,
			reason)
	}
	if rec := f.row(t, id); rec.State != PRStateRejected || rec.DecidedAt == nil {
		t.Fatalf("row = %+v, want rejected", rec)
	}
	want := map[string]int{PRStateOpen: 1, PRStateRejected: 1}
	if got := f.audits(t, id); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audits = %v, want %v", got, want)
	}
}

func TestApprovalPR_DecidedElsewhereSupersedesAndClosesThePR(t *testing.T) {
	f := newPRFixture(t)
	id := f.queueChange(t, "ANALYZE public.orders", "safe")
	f.sync(t)
	if _, err := f.queue.Approve(context.Background(), "orders", id,
		f.users["carol@example.com"], "ANALYZE public.orders"); err != nil {
		t.Fatal(err)
	}
	f.sync(t)
	rec := f.row(t, id)
	if rec.State != PRStateSuperseded || !strings.Contains(rec.Detail, "already decided") {
		t.Fatalf("row = %+v, want superseded, already decided", rec)
	}
	if closed := f.host.Closed(); len(closed) != 1 || closed[0] != 1 {
		t.Fatalf("closed PRs = %v, want [1]", closed)
	}
	if c := f.host.Comments(); len(c) != 1 || !strings.Contains(c[0], "already decided") {
		t.Fatalf("comments = %q, want one saying already decided", c)
	}
	if runs, _ := f.queue.executions(); runs != 1 {
		t.Fatalf("executions = %d, want only the app's 1", runs)
	}
}

func TestApprovalPR_ExpiryClosesThePRAndLeavesTheChangePending(t *testing.T) {
	f := newPRFixture(t)
	id := f.queueChange(t, "ANALYZE public.orders", "safe")
	f.sync(t)
	rec := f.row(t, id)
	if got := rec.ExpiresAt.Sub(f.now); got < 71*time.Hour || got > 73*time.Hour {
		t.Fatalf("expires in %s, want about 72h", got)
	}
	f.now = rec.ExpiresAt.Add(-time.Second)
	f.sync(t)
	if f.row(t, id).State != PRStateOpen {
		t.Fatal("a PR one second before its expiry was closed")
	}
	f.now = rec.ExpiresAt.Add(time.Second)
	f.sync(t)
	if got := f.row(t, id); got.State != PRStateExpired {
		t.Fatalf("row after expiry = %+v, want expired", got)
	}
	if closed := f.host.Closed(); len(closed) != 1 {
		t.Fatalf("closed PRs = %v, want the expired one", closed)
	}
	if status, _ := f.status(t, id); status != "pending" {
		t.Fatalf("queue item = %s, want still pending in pg_sage", status)
	}
	f.sync(t)
	if n := len(f.host.Opened()); n != 1 {
		t.Fatalf("an expired PR's change got a second PR: %d opened", n)
	}
}

func TestApprovalPR_HighTierAndAboveCapOpenNothing(t *testing.T) {
	f := newPRFixture(t)
	high := f.queueChange(t, "VACUUM FULL public.orders", "high")
	f.prs.Config.ChatCap = "safe"
	moderate := f.queueChange(t, "REINDEX INDEX CONCURRENTLY public.i", "moderate")
	safe := f.queueChange(t, "ANALYZE public.orders", "safe")
	res := f.sync(t)
	if res.Opened != 1 || len(f.host.Opened()) != 1 {
		t.Fatalf("opened %d PRs, want only the safe change's", len(f.host.Opened()))
	}
	for _, id := range []int{high, moderate} {
		rec, err := LookupApprovalPR(context.Background(), f.pool, "orders", id)
		if err != nil || rec != nil {
			t.Fatalf("change %d above the cap has a PR row %+v (%v)", id, rec, err)
		}
	}
	if f.row(t, safe).State != PRStateOpen {
		t.Fatal("the safe change has no open PR")
	}
}

func TestApprovalPR_MergeOfAChangeNowAboveTheCapIsRefused(t *testing.T) {
	f := newPRFixture(t)
	id := f.queueChange(t, "ANALYZE public.orders", "moderate")
	f.sync(t)
	f.prs.Config.ChatCap = "safe"
	f.host.Merge(prRepo, 1, "octo-op", nil)
	f.sync(t)
	if runs, _ := f.queue.executions(); runs != 0 {
		t.Fatal("a merge above the current cap ran the change")
	}
	if rec := f.row(t, id); rec.State != PRStateRefused ||
		!strings.Contains(rec.Detail, "chat approval cap") {
		t.Fatalf("row = %+v, want refused by the cap", rec)
	}
}

func TestApprovalPR_NoRepoOpensNothing(t *testing.T) {
	f := newPRFixture(t)
	f.prs.Config.Repo = ""
	f.queueChange(t, "ANALYZE public.orders", "safe")
	res, err := f.prs.Sync(context.Background())
	if err != nil || res != (SyncResult{}) {
		t.Fatalf("sync without a repo = %+v, %v; want nothing", res, err)
	}
	if f.host.Calls() != 0 {
		t.Fatalf("the code host was called %d times without a repo", f.host.Calls())
	}
	if n := f.count(t, `SELECT count(*) FROM sage.approval_prs`); n != 0 {
		t.Fatalf("approval PR rows = %d, want 0", n)
	}
}

func TestApprovalPR_HostErrorRetriesWithBackoffThenOpens(t *testing.T) {
	f := newPRFixture(t)
	id := f.queueChange(t, "ANALYZE public.orders", "safe")
	f.host.FailOpenPR(fmt.Errorf("%w: codehost fake: POST /pulls: HTTP 503: down",
		codehost.ErrUnavailable))
	res := f.sync(t)
	rec := f.row(t, id)
	if res.Failed != 1 || rec.State != PRStateOpenFailed ||
		rec.Detail != "PR not opened: HTTP 503" || rec.Attempts != 1 {
		t.Fatalf("after a 503: %+v (%+v), want open_failed, PR not opened: HTTP 503", rec,
			res)
	}
	if rec.NextAttemptAt == nil || !rec.NextAttemptAt.After(f.now) {
		t.Fatalf("next attempt %v is not after now %v", rec.NextAttemptAt, f.now)
	}
	f.sync(t)
	if got := f.row(t, id); got.Attempts != 1 {
		t.Fatalf("retried before the backoff passed: %d attempts", got.Attempts)
	}
	firstDelay := rec.NextAttemptAt.Sub(f.now)
	f.now = rec.NextAttemptAt.Add(time.Second)
	f.sync(t)
	second := f.row(t, id)
	if second.State != PRStateOpenFailed || second.Attempts != 2 ||
		second.NextAttemptAt == nil {
		t.Fatalf("second failure = %+v, want open_failed with 2 attempts", second)
	}
	if delay := second.NextAttemptAt.Sub(f.now); delay <= firstDelay {
		t.Fatalf("second backoff %s is not longer than the first %s", delay, firstDelay)
	}
	f.host.FailOpenPR(nil)
	f.now = second.NextAttemptAt.Add(time.Second)
	f.sync(t)
	if got := f.row(t, id); got.State != PRStateOpen || got.Detail != "" {
		t.Fatalf("after recovery = %+v, want open with no detail", got)
	}
}
