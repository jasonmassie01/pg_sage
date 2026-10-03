package lint

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// static.md F11/F12: lint rules read the whole catalog with no server
// timeout. Each rule now reads through catalogread under
// safety.query_timeout_ms; a slow rule is cut off, recorded as failed (its
// open findings are not resolved), and the scan goes on to the next rule.

type lintLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lintLog) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func TestScanReport_SlowRulesCutOffAndScanContinues(t *testing.T) {
	pool, _ := requireDB(t)
	logs := &lintLog{}
	l := New(pool, &config.SchemaLintConfig{}, 170000, logs.log)
	l.SetCatalogReadTimeouts(catalogread.Timeouts{Statement: 200 * time.Millisecond})
	var calls atomic.Int32
	slow := catalogread.WithBeforeStatement(context.Background(),
		func(ctx context.Context, tx pgx.Tx) error {
			calls.Add(1)
			_, err := tx.Exec(ctx, "SELECT pg_sleep(10)")
			return err
		})
	start := time.Now()
	findings, failed, err := l.ScanReport(slow)
	if err != nil || len(findings) != 0 {
		t.Fatalf("scan = %d findings (%v), want none and no error", len(findings), err)
	}
	if len(failed) != len(l.rules) || int(calls.Load()) != len(l.rules) {
		t.Fatalf("%d of %d rules failed after %d bounded reads, want every rule "+
			"cut off once", len(failed), len(l.rules), calls.Load())
	}
	if el := time.Since(start); el > time.Duration(len(l.rules))*time.Second {
		t.Fatalf("scan of %d timed-out rules took %s", len(l.rules), el)
	}
	timeouts := 0
	for _, line := range logs.lines {
		if strings.Contains(line, "statement timeout") {
			timeouts++
		}
	}
	if timeouts != len(l.rules) {
		t.Fatalf("%d logged statement timeouts for %d rules", timeouts, len(l.rules))
	}
}

func TestLinter_CatalogReadTimeouts(t *testing.T) {
	l := New(nil, &config.SchemaLintConfig{}, 170000, func(string, string, ...any) {})
	if l.timeouts != catalogread.Default() {
		t.Fatalf("default timeouts = %+v", l.timeouts)
	}
	r := NewRunner(nil, &config.SchemaLintConfig{}, 170000, "db",
		func(string, string, ...any) {})
	want := catalogread.Timeouts{Statement: time.Second}
	r.SetCatalogReadTimeouts(want)
	if r.linter.timeouts != want {
		t.Fatalf("runner did not pass timeouts: %+v", r.linter.timeouts)
	}
}
