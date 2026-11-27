package testdb

import (
	"io"
	"os"
	"strings"
	"testing"
)

// ci-10: a package run without SAGE_TEST_DATABASE_URL says, once, that its
// database tests will skip, and still returns the tests' own exit code.
//
// No concurrent-access tests: Run is the one-shot TestMain path of a
// package process.

// captureStderr runs fn with os.Stderr redirected into a pipe and returns
// what fn wrote there.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		body, _ := io.ReadAll(r)
		done <- string(body)
	}()
	defer func() { os.Stderr = saved }()
	fn()
	os.Stderr = saved
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	return <-done
}

func TestRunWithoutDesignationPrintsOneBannerAndKeepsTheExitCode(t *testing.T) {
	const want = "testdb: SAGE_TEST_DATABASE_URL unset; " +
		"database tests in internal/widgets will skip\n"
	for _, unset := range []string{"", "   "} {
		for _, exit := range []int{0, 3} {
			t.Setenv(EnvName, unset)
			calls := 0
			var code int
			got := captureStderr(t, func() {
				code = Run(func() int {
					calls++
					if dsn := os.Getenv(EnvName); dsn != disabledDSN {
						t.Errorf("%s inside Run = %q, want the disabled DSN", EnvName, dsn)
					}
					return exit
				}, "internal/widgets")
			})
			if code != exit || calls != 1 {
				t.Errorf("unset=%q: Run returned %d after %d calls, want %d after 1",
					unset, code, calls, exit)
			}
			if got != want {
				t.Errorf("unset=%q exit=%d: stderr = %q, want exactly %q",
					unset, exit, got, want)
			}
		}
	}
}

// A designated but unparseable DSN is a setup failure, not the unset case:
// no "will skip" banner, and the tests never run.
func TestRunWithAnInvalidDesignationPrintsNoSkipBanner(t *testing.T) {
	t.Setenv(EnvName, "://invalid")
	called := false
	var code int
	got := captureStderr(t, func() {
		code = Run(func() int {
			called = true
			return 0
		}, "internal/widgets")
	})
	if code != 1 || called {
		t.Fatalf("Run returned %d, called=%v; want 1 without running the tests", code, called)
	}
	if strings.Contains(got, "will skip") || !strings.Contains(got, "test fixture setup failed") {
		t.Fatalf("stderr = %q, want only the setup failure", got)
	}
}
