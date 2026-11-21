package gitops

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// Review case of cli-02b on a real server: a merge at or after the
// request's expiry approves nothing, even when pg_sage polls it late.

func TestApprovalPR_MergeAfterExpiryIsRefused(t *testing.T) {
	cases := []struct {
		name     string
		mergedAt func(expires time.Time) *time.Time
		pollAt   func(expires time.Time) time.Time
		approved bool
	}{
		{name: "merged before expiry, polled after", approved: true,
			mergedAt: func(e time.Time) *time.Time { m := e.Add(-time.Minute); return &m },
			pollAt:   func(e time.Time) time.Time { return e.Add(time.Hour) }},
		{name: "merged at expiry",
			mergedAt: func(e time.Time) *time.Time { return &e },
			pollAt:   func(e time.Time) time.Time { return e.Add(time.Minute) }},
		{name: "no merge time, polled after expiry",
			mergedAt: func(time.Time) *time.Time { return nil },
			pollAt:   func(e time.Time) time.Time { return e.Add(time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPRFixture(t)
			id := f.queueChange(t, "ANALYZE public.orders", "safe")
			f.sync(t)
			expires := f.row(t, id).ExpiresAt
			f.host.Merge(prRepo, 1, "octo-op", nil)
			f.host.SetState(prRepo, codehost.PRStatus{Number: 1, State: codehost.PRMerged,
				MergedBy: "octo-op", MergeSHA: "merge-1", MergedAt: tc.mergedAt(expires)})
			f.now = tc.pollAt(expires)
			f.sync(t)
			runs, _ := f.queue.executions()
			rec := f.row(t, id)
			if tc.approved {
				if runs != 1 || rec.State != PRStateApproved {
					t.Fatalf("runs %d, row %+v, want approved once", runs, rec)
				}
				return
			}
			if runs != 0 || rec.State != PRStateRefused ||
				!strings.Contains(rec.Detail, "expired") {
				t.Fatalf("runs %d, row %+v, want refused as expired", runs, rec)
			}
			if status, _ := f.status(t, id); status != "pending" {
				t.Fatalf("queue item = %s, want still pending in pg_sage", status)
			}
		})
	}
}
