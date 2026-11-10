package executor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// No concurrent access tests here: the options and the session helpers
// keep no shared state; two tagged sessions at once run on a real server
// in action_tag_db_test.go.

const taggedSet = "SET application_name = 'pg_sage action:42'"

// Happy path: a tagged top-level session names its decision after the
// timeouts and search_path, and resets it with the other settings.
func TestActionTagTopLevelSetsAndResetsApplicationName(t *testing.T) {
	sess := &fakeSession{}
	var duringRun []string
	err := runTopLevelSession(context.Background(), sess, time.Minute,
		applyDDLOpts([]DDLOption{WithLockTimeout(5000), WithActionTag(42)}),
		func(pooledExecutor) error {
			duringRun = slices.Clone(sess.execs)
			return nil
		})
	if err != nil {
		t.Fatalf("runTopLevelSession: %v", err)
	}
	wantSet := append(slices.Clone(pinnedTopLevelSet), taggedSet)
	if !slices.Equal(duringRun, wantSet) {
		t.Fatalf("settings before run = %q, want %q", duringRun, wantSet)
	}
	wantAll := append(slices.Clone(wantSet), topLevelResets...)
	wantAll = append(wantAll, "RESET application_name")
	if !slices.Equal(sess.execs, wantAll) {
		t.Fatalf("session statements = %q, want %q", sess.execs, wantAll)
	}
	if sess.released != 1 || sess.discarded != 0 {
		t.Fatalf("released=%d discarded=%d, want released once", sess.released,
			sess.discarded)
	}
}

// Nil/zero and invalid input: decision id 0 or below tags nothing; the
// session keeps the pool's application_name and never touches it.
func TestActionTagZeroOrNegativeLeavesApplicationName(t *testing.T) {
	for _, id := range []int64{0, -7} {
		o := applyDDLOpts([]DDLOption{WithActionTag(id)})
		if o.actionTag != 0 {
			t.Fatalf("WithActionTag(%d) tag = %d, want none", id, o.actionTag)
		}
		sess := &fakeSession{}
		if err := runTopLevelSession(context.Background(), sess, time.Minute, o,
			func(pooledExecutor) error { return nil }); err != nil {
			t.Fatalf("runTopLevelSession: %v", err)
		}
		for _, s := range sess.execs {
			if strings.Contains(s, "application_name") {
				t.Fatalf("untagged session ran %q", s)
			}
		}
	}
}

// Error propagation: a failed RESET application_name discards the
// connection, so no pooled session keeps a decision's name.
func TestActionTagFailedResetDiscardsSession(t *testing.T) {
	sess := &fakeSession{failOn: "RESET application_name"}
	err := runTopLevelSession(context.Background(), sess, time.Minute,
		applyDDLOpts([]DDLOption{WithActionTag(42)}), func(pooledExecutor) error {
			return nil
		})
	if !errors.Is(err, errFakeSession) || !strings.Contains(err.Error(),
		"resetting application_name") {
		t.Fatalf("error = %v, want the application_name reset failure", err)
	}
	if sess.discarded != 1 || sess.released != 0 {
		t.Fatalf("released=%d discarded=%d, want discarded", sess.released, sess.discarded)
	}
}

// Error propagation: a failed SET application_name stops the action before
// it runs, says which setting failed, and still resets the session.
func TestActionTagFailedSetStopsAction(t *testing.T) {
	sess := &fakeSession{failOn: taggedSet}
	ran := false
	err := runTopLevelSession(context.Background(), sess, time.Minute,
		applyDDLOpts([]DDLOption{WithActionTag(42)}), func(pooledExecutor) error {
			ran = true
			return nil
		})
	if ran {
		t.Fatal("the action ran on a session whose tag was not set")
	}
	if !errors.Is(err, errFakeSession) || !strings.Contains(err.Error(),
		"setting application_name") {
		t.Fatalf("error = %v, want the application_name set failure", err)
	}
	if !slices.Contains(sess.execs, "RESET application_name") || sess.released != 1 {
		t.Fatalf("statements %q released=%d: want the session reset and released",
			sess.execs, sess.released)
	}
}

// State transition (transaction path): SET LOCAL names the decision for the
// transaction only, after search_path.
func TestActionTagInTransactionUsesSetLocal(t *testing.T) {
	sess := &fakeSession{}
	o := applyDDLOpts([]DDLOption{WithLockTimeout(800), WithActionTag(9)})
	if err := setLocalSessionScope(context.Background(), sess, time.Second, o); err != nil {
		t.Fatalf("setLocalSessionScope: %v", err)
	}
	want := []string{"SET LOCAL statement_timeout = 1000", "SET LOCAL lock_timeout = '800ms'",
		"SET LOCAL search_path = pg_catalog, pg_temp",
		"SET LOCAL application_name = 'pg_sage action:9'"}
	if !slices.Equal(sess.execs, want) {
		t.Fatalf("transaction settings = %q, want %q", sess.execs, want)
	}
	failing := &fakeSession{failOn: want[3]}
	err := setLocalSessionScope(context.Background(), failing, time.Second, o)
	if !errors.Is(err, errFakeSession) || !strings.Contains(err.Error(), "application_name") {
		t.Fatalf("failed SET LOCAL application_name: error = %v", err)
	}
}

// Every statement Apply runs under a decision carries its tag with the
// lock timeout; a statement with no recorded decision carries none.
func TestLockOptionCarriesDecisionTag(t *testing.T) {
	e := &Executor{}
	sql := "CREATE INDEX CONCURRENTLY idx_a ON public.t (a)"
	o := applyDDLOpts([]DDLOption{e.lockOption(sql, ActionPolicyDecision{DecisionID: 31})})
	if o.actionTag != 31 || o.lockTimeoutMs <= 0 {
		t.Fatalf("lockOption with decision 31 = %+v, want tag 31 and a lock timeout", o)
	}
	o = applyDDLOpts([]DDLOption{e.lockOption(sql, ActionPolicyDecision{})})
	if o.actionTag != 0 || o.lockTimeoutMs <= 0 {
		t.Fatalf("lockOption without a decision = %+v, want no tag", o)
	}
}
