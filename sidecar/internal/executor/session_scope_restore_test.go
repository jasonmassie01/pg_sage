package executor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// security-01c-r: the session's search_path is read before the action and
// that exact value is restored after it, so a pinned pool's session goes
// back pinned and any other session goes back as it was.
func TestSessionScopeRestoresThePriorSearchPath(t *testing.T) {
	for _, prior := range []string{"pg_catalog, pg_temp", `"$user", public`, "app, public"} {
		sess := &fakeSession{path: prior}
		err := runTopLevelSession(context.Background(), sess, time.Minute, ddlOpts{},
			func(pooledExecutor) error { return nil })
		if err != nil {
			t.Fatalf("prior %q: runTopLevelSession: %v", prior, err)
		}
		i := slices.Index(sess.execs, restoreSearchPathSQL)
		if i < 0 || sess.reads != 1 {
			t.Fatalf("prior %q: restore at %d after %d reads in %q", prior, i, sess.reads,
				sess.execs)
		}
		if got := sess.args[i]; len(got) != 1 || got[0] != prior {
			t.Fatalf("restored search_path args = %v, want [%q]", got, prior)
		}
		if sess.released != 1 || sess.discarded != 0 {
			t.Fatalf("released=%d discarded=%d, want released", sess.released, sess.discarded)
		}
	}
}

// Error propagation: a session whose search_path cannot be read runs
// nothing, sets nothing, and is discarded (its state is unknown).
func TestSessionScopeUnreadablePathDiscardsSession(t *testing.T) {
	sess := &fakeSession{readErr: errFakeSession}
	ran := false
	err := runTopLevelSession(context.Background(), sess, time.Minute, ddlOpts{},
		func(pooledExecutor) error { ran = true; return nil })
	if ran || !errors.Is(err, errFakeSession) ||
		!strings.Contains(err.Error(), "reading search_path") {
		t.Fatalf("ran=%v err=%v, want no run and the read failure named", ran, err)
	}
	if len(sess.execs) != 0 || sess.discarded != 1 || sess.released != 0 {
		t.Fatalf("execs=%q discarded=%d released=%d, want nothing run and discarded",
			sess.execs, sess.discarded, sess.released)
	}
}
