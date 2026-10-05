package firstlook

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The first look's own budget against the session's statement_timeout, and
// the one retry of checks that degraded with a transient error. A real
// catalog read is slowed by holding pg_index (which only the index step
// reads) in ACCESS EXCLUSIVE mode from another session.

// lockIndexCatalog holds pg_catalog.pg_index exclusively until release is
// called (or the test ends); the fixture database is this package's own.
func lockIndexCatalog(t *testing.T, ctx context.Context, admin *pgxpool.Pool) func() {
	t.Helper()
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE pg_catalog.pg_index IN ACCESS EXCLUSIVE MODE"); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("lock pg_index: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

// releaseAfter releases the lock after d, while the caller's first look
// waits on it.
func releaseAfter(release func(), d time.Duration) {
	go func() {
		time.Sleep(d)
		release()
	}()
}

var indexRules = []string{RuleInvalidIndex, RuleDuplicateIndex, RuleNeverScannedIndex,
	RuleUnindexedFK}

// sessionTimeoutPool connects with a statement_timeout SET on every new
// session, the way a pg_sage component's session-level SET would leave it.
func sessionTimeoutPool(t *testing.T, ctx context.Context, admin *pgxpool.Pool,
	ms string) *pgxpool.Pool {
	t.Helper()
	cfg := admin.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET statement_timeout = "+ms)
		return err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with session timeout: %v", err)
	}
	t.Cleanup(p.Close)
	var setting, source string
	if err := p.QueryRow(ctx, `SELECT setting, source FROM pg_catalog.pg_settings
		WHERE name = 'statement_timeout'`).Scan(&setting, &source); err != nil {
		t.Fatalf("read session timeout: %v", err)
	}
	if setting != ms || source != "session" {
		t.Fatalf("session timeout = %s (%s), want %s (session)", setting, source, ms)
	}
	return p
}

// pg_sage's own 500 ms on the session does not clamp the first look: with
// pg_index held for 1.5 s the index checks still finish under the 5 s
// budget instead of degrading.
func TestRunReplacesASessionTimeoutWithItsOwnBudget(t *testing.T) {
	admin, ctx := livePool(t)
	s := seedProblems(t, ctx, admin)
	p := sessionTimeoutPool(t, ctx, admin, "500")
	if _, err := Run(ctx, p, testOptions("app")); err != nil { // warm the catalog caches
		t.Fatalf("warm-up run: %v", err)
	}
	release := lockIndexCatalog(t, ctx, admin)
	releaseAfter(release, 1500*time.Millisecond)
	r, err := Run(ctx, p, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.StatementTimeoutMS != int(DefaultStatementTimeout/time.Millisecond) {
		t.Fatalf("statement timeout = %d ms, want the first look's own %v",
			r.StatementTimeoutMS, DefaultStatementTimeout)
	}
	if r.DurationMS < 1000 {
		t.Fatalf("run took %d ms: it did not wait on pg_index, the test proves nothing",
			r.DurationMS)
	}
	for _, rule := range indexRules {
		if c := checkStatus(r, rule); c.Status == CheckDegraded || c.Status == "" {
			t.Fatalf("check %s = %+v, want it to finish under the 5 s budget", rule, c)
		}
	}
	if findItem(r, RuleDuplicateIndex, s+".child_a_two") == nil {
		t.Fatalf("duplicate index missing: %+v", r.Items)
	}
	if len(r.Retryable) != 0 {
		t.Fatalf("retryable = %v, want none", r.Retryable)
	}
}

// operatorRolePool connects as a pg_monitor role whose statement_timeout
// the operator set with ALTER ROLE.
func operatorRolePool(t *testing.T, ctx context.Context, admin *pgxpool.Pool,
	ms string) *pgxpool.Pool {
	t.Helper()
	mon := monitorRole(t, ctx, admin)
	var role string
	if err := mon.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatalf("current user: %v", err)
	}
	execAll(t, ctx, admin, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+
		" SET statement_timeout = "+ms)
	mon.Reset() // new sessions pick up the role setting
	var setting, source string
	if err := mon.QueryRow(ctx, `SELECT setting, source FROM pg_catalog.pg_settings
		WHERE name = 'statement_timeout'`).Scan(&setting, &source); err != nil {
		t.Fatalf("read role timeout: %v", err)
	}
	if setting != ms || source != "user" {
		t.Fatalf("role timeout = %s (%s), want %s (user)", setting, source, ms)
	}
	return mon
}

// A lower statement_timeout the operator set on the role is kept, the
// degraded note says whose limit it was, and the retry (once the catalog
// is free) completes the checks with the same limit.
func TestRunKeepsAnOperatorRoleTimeoutAndRetriesOnce(t *testing.T) {
	admin, ctx := livePool(t)
	s := seedProblems(t, ctx, admin)
	mon := operatorRolePool(t, ctx, admin, "300")
	if _, err := Run(ctx, mon, testOptions("app")); err != nil {
		t.Fatalf("warm-up run: %v", err)
	}
	release := lockIndexCatalog(t, ctx, admin)
	r, err := Run(ctx, mon, testOptions("app"))
	release()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.StatementTimeoutMS != 300 {
		t.Fatalf("statement timeout = %d ms, want the role's 300", r.StatementTimeoutMS)
	}
	for _, rule := range indexRules {
		c := checkStatus(r, rule)
		if c.Status != CheckDegraded || !strings.Contains(c.Note, "300 ms") ||
			!strings.Contains(c.Note, "role") {
			t.Fatalf("check %s = %+v, want degraded by the role's 300 ms", rule, c)
		}
		if !slices.Contains(r.Retryable, rule) {
			t.Fatalf("retryable = %v, want %s", r.Retryable, rule)
		}
	}
	if c := checkStatus(r, RuleXIDRunway); c.Status == CheckDegraded || c.Retried {
		t.Fatalf("xid check = %+v: only the index step waited on pg_index", c)
	}

	retried, err := Retry(ctx, mon, testOptions("app"), r)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, rule := range indexRules {
		c := checkStatus(retried, rule)
		if c.Status == CheckDegraded || !c.Retried ||
			!strings.Contains(c.Note, "first attempt") {
			t.Fatalf("retried check %s = %+v, want it done, marked and the first "+
				"failure kept", rule, c)
		}
	}
	if findItem(retried, RuleDuplicateIndex, s+".child_a_two") == nil {
		t.Fatalf("duplicate index missing after the retry: %+v", retried.Items)
	}
	if retried.StatementTimeoutMS != 300 || len(retried.Retryable) != 0 {
		t.Fatalf("retried report timeout %d retryable %v", retried.StatementTimeoutMS,
			retried.Retryable)
	}
	if c := checkStatus(retried, RuleXIDRunway); c.Retried ||
		c != checkStatus(r, RuleXIDRunway) {
		t.Fatalf("xid check changed by the retry: %+v -> %+v", checkStatus(r, RuleXIDRunway), c)
	}
	if len(retried.Checks) != len(r.Checks) || retried.ID != r.ID ||
		!retried.StartedAt.Equal(r.StartedAt) {
		t.Fatalf("retry must update the same report: %d checks (was %d), id %d (was %d)",
			len(retried.Checks), len(r.Checks), retried.ID, r.ID)
	}
}

// A retry that times out again stays degraded, is marked retried and is
// not offered for another retry.
func TestRetryThatTimesOutAgainStaysDegraded(t *testing.T) {
	admin, ctx := livePool(t)
	seedProblems(t, ctx, admin)
	opts := testOptions("app")
	opts.StatementTimeout = 300 * time.Millisecond
	if _, err := Run(ctx, admin, opts); err != nil {
		t.Fatalf("warm-up run: %v", err)
	}
	release := lockIndexCatalog(t, ctx, admin)
	defer release()
	r, err := Run(ctx, admin, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c := checkStatus(r, RuleDuplicateIndex); c.Status != CheckDegraded {
		t.Fatalf("first attempt = %+v, want degraded", c)
	}
	retried, err := Retry(ctx, admin, opts, r)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	c := checkStatus(retried, RuleDuplicateIndex)
	if c.Status != CheckDegraded || !c.Retried || !strings.Contains(c.Note, "300 ms") ||
		!strings.Contains(c.Note, "first attempt") {
		t.Fatalf("retried check = %+v, want degraded again and marked", c)
	}
	if len(retried.Retryable) != 0 {
		t.Fatalf("retryable after the retry = %v: a check is retried only once",
			retried.Retryable)
	}
}

func TestRetryErrorsAndNoOp(t *testing.T) {
	prev := Report{Database: "app", Retryable: []string{RuleDuplicateIndex}}
	if _, err := Retry(context.Background(), nil, Options{}, prev); !errors.Is(err,
		ErrNoPool) {
		t.Fatalf("nil pool err = %v, want ErrNoPool", err)
	}
	pool, _ := livePool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Retry(ctx, pool, Options{}, prev); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx err = %v, want context.Canceled", err)
	}
	// Nothing to retry: the report comes back unchanged, even on a dead ctx.
	done := Report{Database: "app", Checks: []Check{{Rule: RuleXIDRunway,
		Status: CheckOK}}}
	got, err := Retry(ctx, pool, Options{}, done)
	if err != nil || len(got.Checks) != 1 || got.Checks[0] != done.Checks[0] {
		t.Fatalf("no-op retry = %+v err %v", got, err)
	}
}
