package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/testsupport/restorecmd"
)

// drill-01c wiring: with drill.command.restore_argv set, drills run the
// operator's restore command (a full copy) instead of clone.provider, and
// the clone reaper destroys their leftover clones with destroy_argv. No
// concurrent access tests: one pass runs at a time on the leader.

// commandDrillCfg is a config whose drills run this test binary as the
// restore program, with the given restore mode.
func commandDrillCfg(t *testing.T, mode string) *config.Config {
	t.Helper()
	c := config.DefaultConfig()
	c.Clone.Provider = "none"
	c.Clone.AllowFullCopy = true
	c.DataDir = t.TempDir()
	c.Drill.Command = config.DrillCommandConfig{RestoreArgv: restorecmd.Argv(mode),
		DestroyArgv: restorecmd.Argv("destroy-ok"),
		EnvAllow:    []string{restorecmd.EnvDSN}}
	t.Setenv(restorecmd.EnvDSN, "postgres://drill@clone.invalid:1/app?connect_timeout=2")
	return c
}

func TestRestoreDrillPassRunsTheRestoreCommandWithoutACloneProvider(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	control := mgr.GetInstance("live").Pool
	withGlobalDrillBlock(t, control, `{"enabled": true, "kinds": ["full_copy"]}`)
	c := commandDrillCfg(t, "fail")
	sched := &drill.Scheduler{}
	started := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, sched)
	sched.Wait()
	if len(started) != 1 || started[0] != "live" {
		t.Fatalf("started %v, want live on the restore command", started)
	}
	var passed bool
	var failure, provider, kind string
	if err := control.QueryRow(context.Background(), `SELECT passed, failure, provider,
		kind FROM sage.restore_drills WHERE database_name = 'live'`).Scan(&passed,
		&failure, &provider, &kind); err != nil {
		t.Fatalf("drill row: %v", err)
	}
	if passed || provider != clone.CommandAdapter || kind != "full_copy" ||
		!strings.Contains(failure, "repo locked") || strings.Contains(failure, "hunter2") {
		t.Fatalf("row = %v %q %s %s, want a failed restore_command drill quoting the "+
			"redacted stderr", passed, failure, provider, kind)
	}
	if got := openUnknown(t, control); len(got) != 0 {
		t.Fatalf("recoverability_unknown %v opened beside a configured restore command", got)
	}
	var scope string
	if err := control.QueryRow(context.Background(), `SELECT scope FROM
		sage.clone_instances WHERE purpose = 'drill'`).Scan(&scope); err != nil ||
		scope != clone.CommandScope(c.Drill.Command.RestoreArgv) {
		t.Fatalf("receipt scope = %q (%v), want the command scope", scope, err)
	}
}

func TestRestoreDrillPassRestoreCommandNeedsAllowFullCopy(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	control := mgr.GetInstance("live").Pool
	withGlobalDrillBlock(t, control, `{"enabled": true, "kinds": ["full_copy"]}`)
	c := commandDrillCfg(t, "ok")
	c.Clone.AllowFullCopy = false
	if got := restoreDrillPass(context.Background(), c, c.Clone, mgr, nil,
		&drill.Scheduler{}); len(got) != 0 {
		t.Fatalf("started %v without clone.allow_full_copy", got)
	}
	reason := openUnknown(t, control)["restore_drill:live"]
	if !strings.Contains(reason, "allow_full_copy") {
		t.Fatalf("recoverability_unknown reason %q, want one naming allow_full_copy", reason)
	}
}

func TestRestoreDrillPassWithoutProviderOrCommandNamesBoth(t *testing.T) {
	mgr, restore := drillWiringFixture(t)
	defer restore()
	c := config.DefaultConfig()
	c.Clone.Provider = "none"
	restoreDrillPass(context.Background(), c, c.Clone, mgr, nil, &drill.Scheduler{})
	reason := openUnknown(t, mgr.GetInstance("live").Pool)["restore_drill:live"]
	if !strings.Contains(reason, "clone.provider") ||
		!strings.Contains(reason, "drill.command.restore_argv") {
		t.Fatalf("reason %q, want it to name clone.provider and drill.command", reason)
	}
}

func TestReapCommandDrillClonesRunsTheDestroyProgram(t *testing.T) {
	pool := reaperPool(t)
	ctx := context.Background()
	c := commandDrillCfg(t, "ok")
	adapter, err := commandDrillAdapter(c, "live", pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	p, err := drill.NewReceiptedProvider(adapter, clone.NewReceiptStore(pool),
		drill.ProviderConfig{Adapter: clone.CommandAdapter,
			Scope: clone.CommandScope(c.Drill.Command.RestoreArgv),
			Deployment: func(ctx context.Context) (string, error) {
				return earned.EnsureDeployment(ctx, pool)
			},
			Budget: drill.Budget{}, AllowFullCopy: true, MaxCreatesPerHour: 60})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := p.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.clone_instances
		SET expires_at = now() - interval '1 minute' WHERE ref = $1`, cl.ID); err != nil {
		t.Fatal(err)
	}
	report := reapCommandDrillClones(ctx, c, pool)
	if len(report.Destroyed) != 1 || receiptStatus(t, pool, cl.ID) != clone.StatusDestroyed {
		t.Fatalf("report %+v, receipt %s: want the expired drill clone destroyed", report,
			receiptStatus(t, pool, cl.ID))
	}
	marker := filepath.Join(commandDrillWorkDir(c), "destroyed-"+cl.ID)
	if got, err := os.ReadFile(marker); err != nil || string(got) != cl.ID {
		t.Fatalf("destroy program record %q, %v: want SAGE_CLONE_ID %s", got, err, cl.ID)
	}
}

// No DB: the startup line names the programs and variable names only.
func TestRestoreCommandStartupLineHasNoValues(t *testing.T) {
	c := commandDrillCfg(t, "ok")
	c.Drill.Command.EnvAllow = []string{"PGBACKREST_REPO1_CIPHER_PASS"}
	t.Setenv("PGBACKREST_REPO1_CIPHER_PASS", "s3cr3t-value")
	line := restoreCommandStartupLine(c.Drill.Command)
	if !strings.Contains(line, c.Drill.Command.RestoreArgv[0]) ||
		!strings.Contains(line, "PGBACKREST_REPO1_CIPHER_PASS") {
		t.Fatalf("startup line %q, want the program and the variable name", line)
	}
	if strings.Contains(line, "s3cr3t-value") || strings.Contains(line, restorecmd.Marker) {
		t.Fatalf("startup line %q leaks a value or the argument list", line)
	}
	if restoreCommandStartupLine(config.DrillCommandConfig{}) != "" {
		t.Fatal("no restore command, no startup line")
	}
}
