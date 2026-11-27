//go:build !cgo

package migration

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Invalid build: without cgo the classification is degraded. It says so
// once per process, however many callers race to classify, and every
// statement goes through the regex rules (a parse is never attempted).
func TestDegradedClassificationLogsOnce(t *testing.T) {
	var calls atomic.Int32
	var msg atomic.Value
	prevLog := logDegraded
	degradedOnce = sync.Once{}
	logDegraded = func(m string) {
		calls.Add(1)
		msg.Store(m)
	}
	t.Cleanup(func() { logDegraded, degradedOnce = prevLog, sync.Once{} })

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := ClassifyStatements(context.Background(), nil,
				"ALTER TABLE app.t SET LOGGED", 180000)
			if !res.Degraded || res.Unrecognized() != 1 {
				t.Errorf("degraded build: %+v", res)
			}
		}()
	}
	wg.Wait()
	_ = NewClassifier().Classify("VACUUM FULL t", 0)
	if n := calls.Load(); n != 1 {
		t.Fatalf("degraded notice logged %d times, want 1", n)
	}
	if m, _ := msg.Load().(string); !strings.Contains(m, "degraded") ||
		!strings.Contains(m, "CGO_ENABLED=1") {
		t.Fatalf("notice %q must say degraded and how to build the parser in", m)
	}
}

// Without the parser, astStatement still fails closed: a parse error is
// unrecognized and keeps every regex match.
func TestASTStatementWithoutParserFailsClosed(t *testing.T) {
	found, known := astStatement("ALTER TABLE t ALTER COLUMN a SET NOT NULL", 170000)
	if known {
		t.Fatal("an unparsed statement is known")
	}
	if ids := ruleIDs(found); len(ids) != 1 || ids[0] != "ddl_set_not_null" {
		t.Fatalf("regex floor lost: %v", ids)
	}
	found, known = astStatement("ALTER TABLE app.t ADD CONSTRAINT c_nn NOT NULL c", 180000)
	if !known || len(found) != 1 || found[0].RuleID != "ddl_not_null_constraint_scan" {
		t.Fatalf("PG 18 NOT NULL text form: %v known %v", ruleIDs(found), known)
	}
}
