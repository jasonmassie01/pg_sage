package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/startup"
)

// warnRecorder captures the formatted warnings reportChecks logs.
type warnRecorder struct{ lines []string }

func (w *warnRecorder) warn(component, msg string, args ...any) {
	w.lines = append(w.lines, component+": "+fmt.Sprintf(msg, args...))
}

func TestReportChecksWarnsGrantWhenQueryTextHidden(t *testing.T) {
	var w warnRecorder
	reportChecks("app", &startup.CheckResult{PGVersionNum: 170000}, nil, w.warn)
	if len(w.lines) != 1 || !strings.Contains(w.lines[0], "GRANT pg_read_all_stats") ||
		!strings.Contains(w.lines[0], `"app"`) {
		t.Fatalf("warnings = %q, want one GRANT pg_read_all_stats warning for app", w.lines)
	}
}

func TestReportChecksQuietWhenVisibleAndChecked(t *testing.T) {
	var w warnRecorder
	reportChecks("app", &startup.CheckResult{QueryTextVisible: true, HasWALColumns: true,
		HasPlanTimeColumns: true}, nil, w.warn)
	if len(w.lines) != 0 {
		t.Fatalf("warnings = %q, want none", w.lines)
	}
}

func TestReportChecksNamesCapabilityCauseAndEffect(t *testing.T) {
	var w warnRecorder
	cause := fmt.Errorf("timeout: %w", context.DeadlineExceeded)
	reportChecks("app", &startup.CheckResult{QueryTextVisible: true,
		CapabilityErrors: map[string]error{startup.CapabilityWALColumns: cause}}, nil, w.warn)
	if len(w.lines) != 1 {
		t.Fatalf("warnings = %q, want one", w.lines)
	}
	for _, want := range []string{`"app"`, "wal_columns", "deadline exceeded", "not collected"} {
		if !strings.Contains(w.lines[0], want) {
			t.Errorf("warning %q missing %q", w.lines[0], want)
		}
	}
}

func TestReportChecksDegradedWarnsOnce(t *testing.T) {
	var w warnRecorder
	reportChecks("app", &startup.CheckResult{PGVersionNum: 160000},
		errors.New("pg_stat_statements check: missing"), w.warn)
	if len(w.lines) != 1 || !strings.Contains(w.lines[0], "degraded") ||
		!strings.Contains(w.lines[0], "missing") {
		t.Fatalf("warnings = %q, want one degraded warning naming the cause", w.lines)
	}
}

func TestWithCapabilityFlagsCarriesStatementsSchema(t *testing.T) {
	got := withCapabilityFlags(&config.Config{}, &startup.CheckResult{PGVersionNum: 170000,
		HasWALColumns: true, HasPlanTimeColumns: true, StatementsSchema: "ext"})
	if got.StatementsSchema != "ext" || !got.HasWALColumns || !got.HasPlanTimeColumns ||
		got.PGVersionNum != 170000 {
		t.Fatalf("runtime config = %+v, want the checks' flags and schema", got)
	}
}

// An override delete rebases on loadFileConfigBase; it must keep the
// extension schema found at startup, or the collector would read
// pg_stat_statements unqualified again after the delete.
func TestFileConfigBaseKeepsStatementsSchema(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := strings.Join([]string{"mode: standalone", "postgres:", "  host: db.example",
		"  database: app", "  user: u", ""}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"pg_sage", "--config", path}
	cfg = config.DefaultConfig()
	withCapabilityFlags(cfg, &startup.CheckResult{PGVersionNum: 170000,
		HasWALColumns: true, HasPlanTimeColumns: true, StatementsSchema: "ext"})
	base, err := loadFileConfigBase()
	if err != nil {
		t.Fatalf("load base: %v", err)
	}
	if base.StatementsSchema != "ext" || !base.HasWALColumns || !base.HasPlanTimeColumns {
		t.Fatalf("base schema=%q wal=%v plan=%v, want the startup facts", base.StatementsSchema,
			base.HasWALColumns, base.HasPlanTimeColumns)
	}
}
