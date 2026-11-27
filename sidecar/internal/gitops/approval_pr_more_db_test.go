package gitops

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// cli-02b on a real server: a refused token turns PR mode off, an
// approval in the app racing a merge runs once, and the token never
// reaches a log, a PR or an audit row.

func TestApprovalPR_UnauthorizedDisablesPRMode(t *testing.T) {
	f := newPRFixture(t)
	first := f.queueChange(t, "ANALYZE public.orders", "safe")
	f.host.FailOpenPR(fmt.Errorf("%w: token cannot write acme/config (HTTP 401) "+
		"[github]: Bad credentials", codehost.ErrForbidden))
	res, err := f.prs.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("result %+v, want one failure", res)
	}
	reason := f.prs.Disabled()
	if !strings.Contains(reason, "HTTP 401") {
		t.Fatalf("Disabled() = %q, want the 401 named", reason)
	}
	if st := f.prs.Status(); st.Enabled || st.Disabled != reason || st.Repo != prRepo {
		t.Fatalf("Status() = %+v, want disabled with the reason", st)
	}
	if rec := f.row(t, first); rec.State != PRStateOpenFailed ||
		rec.Detail != "PR not opened: HTTP 401" {
		t.Fatalf("row = %+v, want open_failed, PR not opened: HTTP 401", rec)
	}
	f.host.FailOpenPR(nil)
	f.queueChange(t, "ANALYZE public.billing", "safe")
	calls := f.host.Calls()
	if res := f.sync(t); res != (SyncResult{}) {
		t.Fatalf("a disabled sync did work: %+v", res)
	}
	if f.host.Calls() != calls || len(f.host.Opened()) != 0 {
		t.Fatal("PR mode kept calling the code host after a 401")
	}
}

// CHECK-01 concurrency: an approval in the app and a merge at the same
// time run the change once; the PR path records the loser.
func TestApprovalPR_AppApprovalRacingAMergeRunsOnce(t *testing.T) {
	for round := range 4 {
		t.Run(fmt.Sprint("round ", round), func(t *testing.T) {
			f := newPRFixture(t)
			id := f.queueChange(t, "ANALYZE public.orders", "safe")
			f.sync(t)
			f.host.Merge(prRepo, 1, "octo-op", nil)
			var wg sync.WaitGroup
			var appErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, appErr = f.queue.Approve(context.Background(), "orders", id,
					f.users["carol@example.com"], "ANALYZE public.orders")
			}()
			go func() {
				defer wg.Done()
				_, _ = f.prs.Sync(context.Background())
			}()
			wg.Wait()
			if runs, _ := f.queue.executions(); runs != 1 {
				t.Fatalf("executions = %d, want exactly 1", runs)
			}
			rec := f.row(t, id)
			switch rec.State {
			case PRStateApproved:
				if appErr == nil {
					t.Fatal("both the merge and the app approved")
				}
			case PRStateSuperseded:
				if appErr != nil || !strings.Contains(rec.Detail, "already decided") {
					t.Fatalf("app error %v, row %+v; want the app to win and the PR "+
						"to record already decided", appErr, rec)
				}
			default:
				t.Fatalf("row state %s, want approved or superseded", rec.State)
			}
			if got := f.audits(t, id); got[rec.State] != 1 || got[PRStateOpen] != 1 {
				t.Fatalf("audits = %v, want one open and one %s", got, rec.State)
			}
		})
	}
}

// CHECK-02: the code-host token never appears in logs, PR bodies, PR
// files, comments, approval PR rows or audit rows, even when the host
// echoes it in an error.
func TestApprovalPR_TokenNeverLeaks(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	f := newPRFixture(t)
	first := f.queueChange(t, "ANALYZE public.orders", "safe")
	f.host.FailOpenPR(fmt.Errorf("%w: codehost: HTTP 502: proxy said Bearer %s",
		codehost.ErrUnavailable, secretToken))
	f.sync(t)
	f.host.FailOpenPR(nil)
	f.now = f.now.Add(f.row(t, first).NextAttemptAt.Sub(f.now) + 1)
	f.sync(t)
	f.host.Merge(prRepo, 1, "view-er", nil)
	f.sync(t)
	var seen []string
	for _, pr := range f.host.Opened() {
		seen = append(seen, pr.Title, pr.Body, pr.CommitMessage, pr.Branch)
		for _, file := range pr.Files {
			seen = append(seen, file.Content)
		}
	}
	seen = append(seen, f.host.Comments()...)
	seen = append(seen, logs.String())
	var rows, audits string
	if err := f.pool.QueryRow(context.Background(), `SELECT
		(SELECT coalesce(string_agg(t::text, '|'), '') FROM sage.approval_prs t),
		(SELECT coalesce(string_agg(detail::text, '|'), '') FROM sage.auth_audit)`).
		Scan(&rows, &audits); err != nil {
		t.Fatal(err)
	}
	seen = append(seen, rows, audits)
	if !strings.Contains(audits, "PR not opened: HTTP 502") {
		t.Fatalf("the failure was not audited: %s", audits)
	}
	for _, s := range seen {
		if strings.Contains(s, secretToken) || strings.Contains(s, "ghp_SECRET") {
			t.Fatalf("the code-host token leaked into %q", s)
		}
	}
}

// The GitOps reconcile ignores principals/users.yaml: it is the approval
// PR login map, not a principal file.
func TestApprovalPR_ReconcileLoadIgnoresTheUsersFile(t *testing.T) {
	f := newPRFixture(t)
	dir, err := Load(f.root)
	if err != nil {
		t.Fatalf("Load with principals/users.yaml: %v", err)
	}
	if len(dir.Principals) != 0 {
		t.Fatalf("users.yaml was read as %d principals", len(dir.Principals))
	}
}
