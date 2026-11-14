package lockqueue

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/testsupport/lockqueue"))
}

// fatalTB records a helper's fatal failure and ends the calling goroutine
// the way testing.T does; cleanups run when the call returns.
type fatalTB struct {
	testing.TB
	mu       sync.Mutex
	fatals   []string
	cleanups []func()
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Logf(string, ...any) {}

func (f *fatalTB) Cleanup(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, fn)
}

func (f *fatalTB) Fatal(args ...any) { f.fail(fmt.Sprint(args...)) }

func (f *fatalTB) Fatalf(format string, args ...any) { f.fail(fmt.Sprintf(format, args...)) }

func (f *fatalTB) fail(msg string) {
	f.mu.Lock()
	f.fatals = append(f.fatals, msg)
	f.mu.Unlock()
	runtime.Goexit()
}

// runFatal calls fn on its own goroutine, runs the cleanups it registered
// (last first) and returns the fatal messages and the cleanup count.
func runFatal(fn func(tb testing.TB)) ([]string, int) {
	tb := &fatalTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(tb)
	}()
	<-done
	tb.mu.Lock()
	cleanups := append([]func(){}, tb.cleanups...)
	fatals := append([]string(nil), tb.fatals...)
	tb.mu.Unlock()
	for k := len(cleanups) - 1; k >= 0; k-- {
		cleanups[k]()
	}
	return fatals, len(cleanups)
}

func TestStartReportsAnUnparsableDSN(t *testing.T) {
	fatals, cleanups := runFatal(func(tb testing.TB) {
		Start(tb, "postgres://user@host:notaport/db", "public.lq_bad", 0)
	})
	if len(fatals) != 1 || !strings.HasPrefix(fatals[0], "lockqueue: parse DSN:") {
		t.Fatalf("fatals = %q, want one parse error", fatals)
	}
	if cleanups != 0 {
		t.Errorf("a failed parse registered %d cleanups", cleanups)
	}
}

// Error propagation: a server that is not there fails the test with the
// DSN's host in the message, before any session or table exists.
func TestStartReportsAConnectFailureWithTheHost(t *testing.T) {
	dsn := "postgres://lq_user@127.0.0.1:1/lq_db?sslmode=disable&connect_timeout=2"
	fatals, cleanups := runFatal(func(tb testing.TB) {
		Start(tb, dsn, "public.lq_bad", 2)
	})
	if len(fatals) != 1 || !strings.HasPrefix(fatals[0], "lockqueue: connect:") ||
		!strings.Contains(fatals[0], "127.0.0.1") {
		t.Fatalf("fatals = %q, want one connect error naming 127.0.0.1", fatals)
	}
	if cleanups != 0 {
		t.Errorf("a failed connect registered %d cleanups", cleanups)
	}
}
