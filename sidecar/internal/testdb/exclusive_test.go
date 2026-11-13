package testdb

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// ci-12: Exclusive gives a test its own database on a second server that
// nothing else uses, or skips with a reason the skip budget never allows,
// or fails closed. These tests need no server: every path they cover ends
// before a connection succeeds.
//
// No state-transition test beyond acquire, use, drop and release, which
// the boundary and concurrency tests in exclusive_db_test.go cover.

// stoppingTB is a testing.TB whose Skip and Fatal stop the calling
// goroutine, as the real ones do, and record their message instead of
// ending the outer test. Cleanups run when the goroutine ends (runTB), the
// way the testing package runs them after a Fatal.
type stoppingTB struct {
	testing.TB
	mu       sync.Mutex
	skips    []string
	fatals   []string
	errors   []string
	cleanups []func()
}

func (r *stoppingTB) Helper() {}

func (r *stoppingTB) record(list *[]string, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*list = append(*list, msg)
}

func (r *stoppingTB) Skipf(format string, args ...any) {
	r.record(&r.skips, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func (r *stoppingTB) Skip(args ...any) {
	r.record(&r.skips, fmt.Sprint(args...))
	runtime.Goexit()
}

func (r *stoppingTB) Fatalf(format string, args ...any) {
	r.record(&r.fatals, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func (r *stoppingTB) Fatal(args ...any) {
	r.record(&r.fatals, fmt.Sprint(args...))
	runtime.Goexit()
}

func (r *stoppingTB) Errorf(format string, args ...any) {
	r.record(&r.errors, fmt.Sprintf(format, args...))
}

func (r *stoppingTB) Cleanup(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, fn)
}

func (r *stoppingTB) runCleanups() {
	r.mu.Lock()
	fns := r.cleanups
	r.cleanups = nil
	r.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// runTB runs body on its own goroutine with rec as its testing.TB and
// returns once body and rec's cleanups have finished.
func runTB(t *testing.T, body func(tb testing.TB)) *stoppingTB {
	t.Helper()
	rec := &stoppingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rec.runCleanups()
		body(rec)
	}()
	<-done
	return rec
}

func TestExclusiveSkipsOnceWhenTheServerIsUnset(t *testing.T) {
	for _, unset := range []string{"", "   ", "\t\n"} {
		t.Setenv(ExclusiveEnvName, unset)
		t.Setenv(EnvName, "postgres://postgres:secret@127.0.0.1:1/postgres?sslmode=disable")
		returned := false
		rec := runTB(t, func(tb testing.TB) {
			Exclusive(tb, "wal volume probe")
			returned = true
		})
		if returned {
			t.Errorf("unset=%q: Exclusive returned instead of skipping", unset)
		}
		if len(rec.skips) != 1 || len(rec.fatals) != 0 || len(rec.errors) != 0 {
			t.Fatalf("unset=%q: skips=%q fatals=%q errors=%q, want exactly one skip",
				unset, rec.skips, rec.fatals, rec.errors)
		}
		want := "exclusive-cluster: wal volume probe needs " + ExclusiveEnvName
		if rec.skips[0] != want {
			t.Errorf("unset=%q: skip = %q, want %q", unset, rec.skips[0], want)
		}
	}
}

// exclusiveFatal runs Exclusive with the two variables set and returns the
// one fatal message it must end with.
func exclusiveFatal(t *testing.T, exclusive, designated string) string {
	t.Helper()
	t.Setenv(ExclusiveEnvName, exclusive)
	t.Setenv(EnvName, designated)
	returned := false
	rec := runTB(t, func(tb testing.TB) {
		Exclusive(tb, "fail closed probe")
		returned = true
	})
	if returned || len(rec.skips) != 0 || len(rec.fatals) != 1 {
		t.Fatalf("returned=%v skips=%q fatals=%q, want exactly one fatal",
			returned, rec.skips, rec.fatals)
	}
	return rec.fatals[0]
}

const unreachableDSN = "postgres://postgres:secret@127.0.0.1:1/postgres" +
	"?sslmode=disable&connect_timeout=2"

func TestExclusiveRejectsAnExclusiveURLThatIsNotPostgres(t *testing.T) {
	for _, raw := range []string{
		"mysql://root:secret@db.example:3306/app",
		"host=db.example port=5432 user=postgres password=secret",
		"postgres://postgres:secret@[::1:5432/postgres",
	} {
		msg := exclusiveFatal(t, raw, unreachableDSN)
		if !strings.Contains(msg, "parse "+ExclusiveEnvName) {
			t.Errorf("%q: fatal %q does not name parse %s", raw, msg, ExclusiveEnvName)
		}
		if strings.Contains(msg, "secret") {
			t.Errorf("%q: fatal %q leaks the password", raw, msg)
		}
	}
}

func TestExclusiveAcceptsBothPostgresSchemes(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		raw := scheme + "://postgres:secret@127.0.0.1:1/postgres" +
			"?sslmode=disable&connect_timeout=2"
		msg := exclusiveFatal(t, raw, unreachableDSN)
		// Parsing passed, so it fails one step later, connecting.
		if strings.Contains(msg, "parse ") ||
			!strings.Contains(msg, "connect exclusive test server") {
			t.Errorf("%s: fatal %q, want the connect failure after a clean parse", scheme, msg)
		}
	}
}

// Without the designated server Exclusive cannot prove the two differ, so
// it fails rather than measure on what may be the shared server.
func TestExclusiveFailsClosedWithoutTheDesignatedServer(t *testing.T) {
	exclusive := "postgres://postgres:secret@db.example:5433/postgres?sslmode=disable"
	for _, designated := range []string{"", "  ", disabledDSN} {
		msg := exclusiveFatal(t, exclusive, designated)
		if !strings.Contains(msg, EnvName) || !strings.Contains(msg, "same cluster") {
			t.Errorf("designated=%q: fatal %q, want it to name %s and the same-cluster check",
				designated, msg, EnvName)
		}
	}
	msg := exclusiveFatal(t, exclusive, "mysql://root:secret@db.example/app")
	if !strings.Contains(msg, "parse "+EnvName) || strings.Contains(msg, "secret") {
		t.Errorf("bad designated URL: fatal %q, want parse %s without the password",
			msg, EnvName)
	}
}

func TestExclusiveNamesTheUnreachableServer(t *testing.T) {
	msg := exclusiveFatal(t, unreachableDSN,
		"postgres://postgres:secret@db.example:5432/postgres?sslmode=disable")
	for _, want := range []string{"connect exclusive test server", "127.0.0.1:1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fatal %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "secret") {
		t.Errorf("fatal %q leaks the password", msg)
	}
}

func TestCreateFixtureNamesTheServerItCouldNotReach(t *testing.T) {
	name := fixtureDatabaseName("internal/testdb", 42)
	for _, label := range []string{"exclusive", "designated"} {
		_, err := createFixture(unreachableDSN, name, label)
		if err == nil {
			t.Fatalf("%s: createFixture reached a closed port", label)
		}
		if !strings.Contains(err.Error(), "connect "+label+" test server") {
			t.Errorf("%s: error %q does not name the %s server", label, err, label)
		}
	}
}
