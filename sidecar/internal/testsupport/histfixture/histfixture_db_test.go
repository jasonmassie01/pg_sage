package histfixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: a Pair is used by one test at a time and the
// process mode it sets is process-wide by design.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/testsupport/histfixture"))
}

// recordingTB keeps a Pair's cleanups and errors so a test can run them
// and read what the Pair reported.
type recordingTB struct {
	*testing.T
	errs     []string
	cleanups []func()
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recordingTB) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

// State transitions: meta mode makes the process meta and refuses an
// unregistered pool; the Pair reports the refusals at cleanup and leaves
// the process monitored.
func TestPairReportsRefusedStatementsInMetaMode(t *testing.T) {
	rec := &recordingTB{T: t}
	p := NewPair(rec)
	p.Switch(rec, histstore.ModeMeta)
	if got := histstore.ProcessMode(); got != histstore.ModeMeta {
		t.Fatalf("process mode in a meta phase = %q, want meta", got)
	}
	_, err := histstore.Resolve(p.Meta).Exec(context.Background(), "SELECT 1")
	if !errors.Is(err, histstore.ErrNotRegistered) {
		t.Fatalf("unregistered pool in meta mode: err = %v, want ErrNotRegistered", err)
	}
	if _, err := histstore.Resolve(p.Monitored).Exec(context.Background(),
		"SELECT count(*) FROM sage.query_store WHERE {db:}"); err != nil {
		t.Fatalf("registered pool in meta mode: %v", err)
	}
	p.Switch(rec, histstore.ModeMonitored)
	if got := histstore.ProcessMode(); got != histstore.ModeMonitored {
		t.Fatalf("process mode after a monitored switch = %q", got)
	}
	rec.runCleanups()
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "1 history statements") {
		t.Fatalf("cleanup errors = %q, want one naming 1 refused statement", rec.errs)
	}
}

func TestPairCleanWithoutRefusals(t *testing.T) {
	rec := &recordingTB{T: t}
	p := NewPair(rec)
	p.Switch(rec, histstore.ModeMeta)
	rec.runCleanups()
	if len(rec.errs) != 0 {
		t.Fatalf("cleanup errors = %q, want none", rec.errs)
	}
	if got := histstore.ProcessMode(); got != histstore.ModeMonitored {
		t.Fatalf("process mode after cleanup = %q, want monitored", got)
	}
}
