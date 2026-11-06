package executor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// No concurrent access tests: one action owns its session under the DDL
// lease, and these helpers keep no shared state between sessions.

var errFakeSession = errors.New("fake session failure")

// fakeSession records every statement and how the session left the pool.
type fakeSession struct {
	execs     []string
	failOn    string
	released  int
	discarded int
}

func (f *fakeSession) Exec(
	_ context.Context, sql string, _ ...any,
) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	if f.failOn != "" && sql == f.failOn {
		return pgconn.CommandTag{}, errFakeSession
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeSession) Release() { f.released++ }
func (f *fakeSession) discard() { f.discarded++ }

var pinnedTopLevelSet = []string{
	"SET statement_timeout = 60000",
	"SET lock_timeout = '5000ms'",
	"SET search_path = pg_catalog, pg_temp",
}

var topLevelResets = []string{
	"RESET statement_timeout",
	"RESET lock_timeout",
	"RESET search_path",
}

func TestSessionScopePinsSearchPathAfterTimeouts(t *testing.T) {
	sess := &fakeSession{}
	var duringRun []string
	err := runTopLevelSession(context.Background(), sess, time.Minute,
		ddlOpts{lockTimeoutMs: 5000}, func(pooledExecutor) error {
			duringRun = slices.Clone(sess.execs)
			return nil
		})
	if err != nil {
		t.Fatalf("runTopLevelSession: %v", err)
	}
	if !slices.Equal(duringRun, pinnedTopLevelSet) {
		t.Fatalf("session settings before run = %q, want %q", duringRun, pinnedTopLevelSet)
	}
	want := append(slices.Clone(pinnedTopLevelSet), topLevelResets...)
	if !slices.Equal(sess.execs, want) {
		t.Fatalf("session statements = %q, want %q", sess.execs, want)
	}
	if sess.released != 1 || sess.discarded != 0 {
		t.Fatalf("released=%d discarded=%d, want the clean session released once",
			sess.released, sess.discarded)
	}
}

// Boundary: without a lock timeout the session still pins search_path and
// still resets lock_timeout (a reset of an unset value is harmless).
func TestSessionScopePinsSearchPathWithoutLockTimeout(t *testing.T) {
	sess := &fakeSession{}
	var duringRun []string
	err := runTopLevelSession(context.Background(), sess, 1500*time.Millisecond,
		ddlOpts{}, func(pooledExecutor) error {
			duringRun = slices.Clone(sess.execs)
			return nil
		})
	if err != nil {
		t.Fatalf("runTopLevelSession: %v", err)
	}
	want := []string{"SET statement_timeout = 1500", "SET search_path = pg_catalog, pg_temp"}
	if !slices.Equal(duringRun, want) {
		t.Fatalf("session settings before run = %q, want %q", duringRun, want)
	}
	if got := sess.execs[len(sess.execs)-3:]; !slices.Equal(got, topLevelResets) {
		t.Fatalf("resets = %q, want %q", got, topLevelResets)
	}
}

// Error propagation: any failed RESET, the new search_path one included,
// discards the connection instead of returning a pinned session to the pool.
func TestSessionScopeFailedResetDiscardsSession(t *testing.T) {
	for _, reset := range topLevelResets {
		t.Run(reset, func(t *testing.T) {
			sess := &fakeSession{failOn: reset}
			ran := false
			err := runTopLevelSession(context.Background(), sess, time.Minute,
				ddlOpts{lockTimeoutMs: 5000}, func(pooledExecutor) error {
					ran = true
					return nil
				})
			if !ran {
				t.Fatal("run was not called")
			}
			if !errors.Is(err, errFakeSession) {
				t.Fatalf("error = %v, want the reset failure", err)
			}
			setting := strings.TrimPrefix(reset, "RESET ")
			if !strings.Contains(err.Error(), "resetting "+setting) {
				t.Fatalf("error %q does not name the failed reset of %s", err, setting)
			}
			if sess.discarded != 1 || sess.released != 0 {
				t.Fatalf("released=%d discarded=%d, want discarded and never released",
					sess.released, sess.discarded)
			}
		})
	}
}

// Error propagation: the action's own error survives a failed reset.
func TestSessionScopeFailedResetKeepsRunError(t *testing.T) {
	errRun := errors.New("ddl failed")
	sess := &fakeSession{failOn: "RESET search_path"}
	err := runTopLevelSession(context.Background(), sess, time.Minute, ddlOpts{},
		func(pooledExecutor) error { return errRun })
	if !errors.Is(err, errRun) || !errors.Is(err, errFakeSession) {
		t.Fatalf("error = %v, want both the run error and the reset error", err)
	}
	if sess.discarded != 1 || sess.released != 0 {
		t.Fatalf("released=%d discarded=%d, want discarded", sess.released, sess.discarded)
	}
}

// A failed pin never runs the statement, and the session is still reset.
func TestSessionScopeFailedPinSkipsRun(t *testing.T) {
	sess := &fakeSession{failOn: "SET search_path = pg_catalog, pg_temp"}
	ran := false
	err := runTopLevelSession(context.Background(), sess, time.Minute,
		ddlOpts{lockTimeoutMs: 5000}, func(pooledExecutor) error {
			ran = true
			return nil
		})
	if ran {
		t.Fatal("run executed although search_path could not be pinned")
	}
	if !errors.Is(err, errFakeSession) || !strings.Contains(err.Error(), "setting search_path") {
		t.Fatalf("error = %v, want the search_path setting failure", err)
	}
	if got := sess.execs[len(sess.execs)-3:]; !slices.Equal(got, topLevelResets) {
		t.Fatalf("resets after a failed pin = %q, want %q", got, topLevelResets)
	}
	if sess.released != 1 || sess.discarded != 0 {
		t.Fatalf("released=%d discarded=%d, want released after clean resets",
			sess.released, sess.discarded)
	}
}

func TestSessionScopeTransactionLocalPin(t *testing.T) {
	sess := &fakeSession{}
	if err := setLocalSessionScope(context.Background(), sess, time.Minute,
		ddlOpts{lockTimeoutMs: 250}); err != nil {
		t.Fatalf("setLocalSessionScope: %v", err)
	}
	want := []string{
		"SET LOCAL statement_timeout = 60000",
		"SET LOCAL lock_timeout = '250ms'",
		"SET LOCAL search_path = pg_catalog, pg_temp",
	}
	if !slices.Equal(sess.execs, want) {
		t.Fatalf("transaction settings = %q, want %q", sess.execs, want)
	}
}

func TestSessionScopeTransactionLocalPinWithoutLockTimeout(t *testing.T) {
	sess := &fakeSession{}
	if err := setLocalSessionScope(context.Background(), sess, time.Second,
		ddlOpts{}); err != nil {
		t.Fatalf("setLocalSessionScope: %v", err)
	}
	want := []string{
		"SET LOCAL statement_timeout = 1000",
		"SET LOCAL search_path = pg_catalog, pg_temp",
	}
	if !slices.Equal(sess.execs, want) {
		t.Fatalf("transaction settings = %q, want %q", sess.execs, want)
	}
}

func TestSessionScopeTransactionLocalPinFailure(t *testing.T) {
	sess := &fakeSession{failOn: "SET LOCAL search_path = pg_catalog, pg_temp"}
	err := setLocalSessionScope(context.Background(), sess, time.Minute, ddlOpts{})
	if !errors.Is(err, errFakeSession) || !strings.Contains(err.Error(), "setting search_path") {
		t.Fatalf("error = %v, want the search_path setting failure", err)
	}
}

// Error propagation: a failed timeout never runs the statement or pins the
// path, names the setting, and the session is still reset and released.
func TestSessionScopeFailedTimeoutSkipsRunAndPin(t *testing.T) {
	for _, set := range pinnedTopLevelSet[:2] {
		t.Run(set, func(t *testing.T) {
			sess := &fakeSession{failOn: set}
			ran := false
			err := runTopLevelSession(context.Background(), sess, time.Minute,
				ddlOpts{lockTimeoutMs: 5000}, func(pooledExecutor) error {
					ran = true
					return nil
				})
			setting := strings.Fields(set)[1]
			if ran || !errors.Is(err, errFakeSession) ||
				!strings.Contains(err.Error(), "setting "+setting) {
				t.Fatalf("ran=%v error=%v, want no run and the %s failure", ran, err, setting)
			}
			if slices.Contains(sess.execs, pinnedTopLevelSet[2]) {
				t.Fatalf("search_path pinned after a failed %s: %q", setting, sess.execs)
			}
			if got := sess.execs[len(sess.execs)-3:]; !slices.Equal(got, topLevelResets) ||
				sess.released != 1 {
				t.Fatalf("resets=%q released=%d, want all resets and one release",
					got, sess.released)
			}
		})
	}
}

func TestSessionScopeTransactionLocalTimeoutFailure(t *testing.T) {
	for _, set := range []string{
		"SET LOCAL statement_timeout = 60000", "SET LOCAL lock_timeout = '250ms'",
	} {
		sess := &fakeSession{failOn: set}
		err := setLocalSessionScope(context.Background(), sess, time.Minute,
			ddlOpts{lockTimeoutMs: 250})
		setting := strings.Fields(set)[2]
		if !errors.Is(err, errFakeSession) || !strings.Contains(err.Error(), "setting "+setting) {
			t.Fatalf("%s: error = %v, want the %s failure", set, err, setting)
		}
		if last := sess.execs[len(sess.execs)-1]; last != set {
			t.Fatalf("%s: statements continued after the failure: %q", set, sess.execs)
		}
	}
}
