package managedparam

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// market-12: a CloudNativePG Cluster changes settings through
// spec.postgresql.parameters. pg_sage proposes a kubectl merge patch, its
// rollback and a GitOps fragment; it never patches the Cluster itself.

// No concurrent access tests here: Build is a pure function; the worker's
// per-Cluster deduplication is covered in worker_cnpg_db_test.go.

func cnpgTarget(flags map[string]string) Target {
	return Target{Provider: "cnpg", Cluster: "pg-main", Namespace: "db", Known: true,
		FlagsKnown: true, Flags: flags}
}

func TestBuildCNPGWorkMemPatchAndRollback(t *testing.T) {
	running := &collector.PGSetting{Name: "work_mem", Setting: "4096", Unit: "kB",
		Source: "configuration file", Context: "user"}
	p, err := Build(intent(t, "cnpg", "work_mem", "64MB"),
		cnpgTarget(map[string]string{"work_mem": "4MB", "max_connections": "200"}), running)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantPatch := `{"spec":{"postgresql":{"parameters":{"work_mem":"64MB"}}}}`
	wantRollback := `{"spec":{"postgresql":{"parameters":{"work_mem":"4MB"}}}}`
	if p.Provider != "cnpg" || p.Mechanism != MechanismClusterSpec ||
		p.Target != "db/pg-main" || p.Cluster != "pg-main" || p.Namespace != "db" ||
		p.Value != "64MB" || p.PGValue != "64MB" || p.CurrentValue != "4MB" ||
		p.RunningValue != "4096" || p.RebootRequired || p.ApplyMethod != ApplyImmediate {
		t.Fatalf("proposal = %+v", p)
	}
	if p.Patch != wantPatch || p.Rollback.Patch != wantRollback {
		t.Fatalf("patch %s rollback %s, want %s and %s", p.Patch, p.Rollback.Patch,
			wantPatch, wantRollback)
	}
	wantCLI := "kubectl patch cluster pg-main -n db --type merge -p '" + wantPatch + "'"
	if p.CLI != wantCLI {
		t.Fatalf("CLI =\n%s\nwant\n%s", p.CLI, wantCLI)
	}
	if p.Rollback.CLI != "kubectl patch cluster pg-main -n db --type merge -p '"+
		wantRollback+"'" || p.Rollback.Value != "4MB" || p.Rollback.Reset {
		t.Fatalf("rollback = %+v", p.Rollback)
	}
	wantGitOps := "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n" +
		"  name: pg-main\n  namespace: db\nspec:\n  postgresql:\n    parameters:\n" +
		"      work_mem: \"64MB\"\n"
	if p.GitOps != wantGitOps {
		t.Fatalf("GitOps =\n%s\nwant\n%s", p.GitOps, wantGitOps)
	}
	if !p.RequiresApproval || p.AutoApply || p.AutoApplyWithheld != AutoApplyWithheld ||
		len(p.Blockers) != 0 || !strings.HasPrefix(p.Fingerprint, "v2:") {
		t.Fatalf("approval and blockers: %+v", p)
	}
	if !hasNote(p, "reload") || hasNote(p, "switchover") {
		t.Fatalf("a reloadable setting needs no restart note: %q", p.Notes)
	}
}

// A key the Cluster does not set is removed again by the rollback (null).
func TestBuildCNPGRollbackRemovesAnUnsetKey(t *testing.T) {
	p, err := Build(intent(t, "cnpg", "random_page_cost", "1.1"), cnpgTarget(nil), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Rollback.Patch != `{"spec":{"postgresql":{"parameters":{"random_page_cost":null}}}}` ||
		!p.Rollback.Reset || p.Rollback.Value != "" || p.CurrentValue != "" {
		t.Fatalf("rollback = %+v, current %q", p.Rollback, p.CurrentValue)
	}
}

func TestBuildCNPGRestartCarriesSwitchoverNote(t *testing.T) {
	target := cnpgTarget(map[string]string{})
	target.UpdateStrategy = "unsupervised"
	p, err := Build(intent(t, "cnpg", "shared_buffers", "4GB"), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !p.RebootRequired || p.ApplyMethod != ApplyRollingRestart || p.Value != "4GB" {
		t.Fatalf("restart proposal = %+v", p)
	}
	for _, want := range []string{"rolling restart", "switchover",
		"primaryUpdateStrategy: unsupervised", "primaryUpdateMethod"} {
		if !hasNote(p, want) {
			t.Errorf("notes %q lack %q", p.Notes, want)
		}
	}
}

func TestBuildCNPGBooleanIsOnOff(t *testing.T) {
	p, err := Build(intent(t, "cnpg", "track_io_timing", "true"), cnpgTarget(nil), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Value != "on" || !strings.Contains(p.Patch, `"track_io_timing":"on"`) {
		t.Fatalf("boolean = %q patch %s", p.Value, p.Patch)
	}
}

// Invalid input: names outside the RFC 1123 label pattern and values with
// a quote never reach a command.
func TestBuildCNPGRejectsUnsafeNames(t *testing.T) {
	in := intent(t, "cnpg", "work_mem", "64MB")
	for _, tc := range []struct{ cluster, namespace string }{
		{"Pg-Main", "db"}, {"pg_main", "db"}, {"pg-main", "db;rm"}, {"-pg", "db"},
		{"pg-", "db"}, {strings.Repeat("a", 64), "db"}, {"pg-main", "Db"},
		{"pg main", "db"}, {"pg-main", "db'x"},
	} {
		target := cnpgTarget(nil)
		target.Cluster, target.Namespace = tc.cluster, tc.namespace
		p, err := Build(in, target, nil)
		if !errors.Is(err, ErrInvalidTarget) || p.CLI != "" {
			t.Errorf("cluster %q namespace %q: err %v, CLI %q", tc.cluster, tc.namespace,
				err, p.CLI)
		}
	}
	if _, err := NewIntent("cnpg", "work_mem", "64'MB", "x"); !errors.Is(err,
		ErrInvalidValue) {
		t.Fatalf("a quoted value must be refused: %v", err)
	}
	ok := cnpgTarget(nil)
	ok.Cluster = strings.Repeat("a", 63)
	if _, err := Build(in, ok, nil); err != nil {
		t.Fatalf("a 63-character cluster name is a valid label: %v", err)
	}
}

// Boundary: the operator fixes some parameters; proposing one is refused.
func TestBuildCNPGFixedParameterIsNotModifiable(t *testing.T) {
	for _, tc := range [][2]string{{"port", "5433"}, {"full_page_writes", "on"},
		{"hot_standby", "on"}, {"wal_log_hints", "on"}} {
		_, err := Build(intent(t, "cnpg", tc[0], tc[1]), cnpgTarget(nil), nil)
		if !errors.Is(err, ErrNotModifiable) || !strings.Contains(err.Error(), tc[0]) {
			t.Errorf("%s: err %v, want ErrNotModifiable naming it", tc[0], err)
		}
	}
}

// Nil/empty: no resolved Cluster gives placeholders and a fill-in rollback.
func TestBuildCNPGUnresolvedUsesPlaceholders(t *testing.T) {
	target := Target{Provider: "cnpg", Unresolved: "cnpg is not configured"}
	p, err := Build(intent(t, "cnpg", "work_mem", "64MB"), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Target != "<namespace>/<cluster>" || !p.placeholderTarget() ||
		!strings.HasPrefix(p.CLI, "kubectl patch cluster <cluster> -n <namespace> ") {
		t.Fatalf("placeholder proposal = %+v", p)
	}
	if p.Rollback.Patch != "" || !strings.HasPrefix(p.Rollback.CLI, "# fill in: ") ||
		!hasNote(p, "cnpg is not configured") {
		t.Fatalf("unknown prior value needs a fill-in rollback: %+v notes %q", p.Rollback,
			p.Notes)
	}
}

// A current value that cannot be quoted withholds the rollback command.
func TestBuildCNPGUnsafePriorValueBlocksRollback(t *testing.T) {
	p, err := Build(intent(t, "cnpg", "work_mem", "64MB"),
		cnpgTarget(map[string]string{"work_mem": "4MB' ; rm"}), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Rollback.Patch != "" || !strings.HasPrefix(p.Rollback.CLI, "# no rollback") ||
		len(p.Blockers) != 1 || p.Rollback.Value != "" ||
		strings.Contains(p.Rollback.CLI, "rm") {
		t.Fatalf("unsafe prior value: rollback %+v blockers %q", p.Rollback, p.Blockers)
	}
	if !strings.HasPrefix(p.CLI, "kubectl patch cluster pg-main") {
		t.Fatalf("the forward patch touches only the proposed key: %q", p.CLI)
	}
}

func TestCNPGDriftComparesInBaseUnits(t *testing.T) {
	target := cnpgTarget(map[string]string{"work_mem": "64MB", "shared_buffers": "4GB",
		"random_page_cost": "1.1", "jit": "off"})
	settings := []collector.PGSetting{
		{Name: "work_mem", Setting: "65536", Unit: "kB", Source: "configuration file"},
		{Name: "shared_buffers", Setting: "524288", Unit: "8kB",
			Source: "configuration file"},
		{Name: "random_page_cost", Setting: "4", Source: "configuration file"},
		{Name: "jit", Setting: "off", Source: "configuration file"},
	}
	drift := DetectDrift(target, settings)
	if len(drift) != 1 || drift[0].Parameter != "random_page_cost" ||
		drift[0].Kind != DriftMismatch || drift[0].Configured != "1.1" {
		t.Fatalf("drift = %+v, want only random_page_cost", drift)
	}
	settings[1].PendingRestart, settings[1].Setting = true, "131072"
	drift = DetectDrift(target, settings)
	if len(drift) != 2 || drift[1].Parameter != "shared_buffers" ||
		drift[1].Kind != DriftPendingReboot {
		t.Fatalf("pending restart drift = %+v", drift)
	}
	if got := DetectDrift(Target{Provider: "cnpg"}, settings); got != nil {
		t.Fatalf("an unresolved Cluster reports no drift: %+v", got)
	}
}

// The "now running" check compares the PostgreSQL form with pg_settings.
func TestCNPGRunningFormIsBaseUnits(t *testing.T) {
	p, err := Build(intent(t, "cnpg", "work_mem", "64MB"), cnpgTarget(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := runningForm(p); !sameValue(got, "65536") {
		t.Fatalf("runningForm = %q, want 65536 (kB)", got)
	}
}

func TestNormalizeProviderCNPG(t *testing.T) {
	for in, want := range map[string]string{"cnpg": "cnpg", " CNPG ": "cnpg",
		"cloudnative-pg": "cnpg", "cloudnativepg": "cnpg"} {
		if got := NormalizeProvider(in); got != want {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", in, got, want)
		}
	}
	if !Supported("cnpg") {
		t.Fatal("cnpg must be supported")
	}
}

func hasNote(p Proposal, sub string) bool {
	for _, n := range p.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
