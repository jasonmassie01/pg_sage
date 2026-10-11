package firstlook

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentposture"
)

// A step whose transaction cannot be reopened is degraded with the reason,
// and the next step opens a transaction of its own. It must not inherit
// the half-opened one, which the failed read aborted: that step would be
// degraded as "current transaction is aborted", an error that is not
// retryable, so a check that would have run fine was never retried (CI,
// PG18: the XID check after a blocked index step).
//
// The session's own statement_timeout (300 ms, SET on the session) ends
// the opening read of pg_settings, which another session holds locked;
// the connection stays usable, unlike after a client-side deadline. The
// posture detectors are left out: the test is about the 9 rules' steps.
func TestFailedReopenDoesNotLeakAnAbortedTransaction(t *testing.T) {
	admin, ctx := livePool(t)
	single := warmSingleConn(t, ctx, sessionTimeoutPool(t, ctx, admin, "300"))
	lock, err := pgx.ConnectConfig(ctx, admin.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect lock session: %v", err)
	}
	t.Cleanup(func() { _ = lock.Close(context.Background()) })
	for _, sql := range []string{"BEGIN",
		"LOCK TABLE pg_catalog.pg_settings IN ACCESS EXCLUSIVE MODE"} {
		if _, err := lock.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	opts := testOptions("app")
	opts.PostureRegistry = agentposture.NewRegistry()
	r, err := Run(ctx, single, opts)
	if err != nil {
		t.Fatalf("run = %v, want a report with degraded checks", err)
	}
	if len(r.Checks) != 9 {
		t.Fatalf("run reported %d checks, want all 9 rules: %+v", len(r.Checks), r.Checks)
	}
	for _, c := range r.Checks {
		if c.Status != CheckDegraded || !strings.Contains(c.Note, "statement timeout") ||
			strings.Contains(c.Note, "aborted") {
			t.Fatalf("check %+v, want degraded by the statement timeout of its own "+
				"(re)open", c)
		}
		if !slices.Contains(r.Retryable, c.Rule) {
			t.Fatalf("retryable = %v, want %s: a timeout is transient", r.Retryable, c.Rule)
		}
	}
	if _, err := lock.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("release pg_settings: %v", err)
	}
	retried, err := Retry(ctx, single, opts, r)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, c := range retried.Checks {
		if c.Status == CheckDegraded || !c.Retried {
			t.Fatalf("retried check %+v, want it done once pg_settings is free", c)
		}
	}
}
