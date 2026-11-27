package clone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/restorecmd"
)

// drill-02b: the restore command provider upgrades a rehearsal clone in
// place through the operator's upgrade_argv (SAGE_CLONE_ID and
// SAGE_TARGET_MAJOR in its environment).

// No concurrent access tests: Upgrade runs one program per call and keeps
// no state between calls.

func upgradeConfig(t *testing.T, mode string, args ...string) CommandConfig {
	t.Helper()
	cfg := commandConfig(t, "ok")
	cfg.UpgradeArgv = restorecmd.Argv(mode, args...)
	return cfg
}

func TestCommandUpgrade_InPlaceKeepsTheCloneAndPassesTheMajor(t *testing.T) {
	cfg := upgradeConfig(t, "upgrade-ok")
	p := newCommand(t, cfg)
	in := Clone{DSN: testCloneDSN, ID: "clone-sage-rehearsal-0a1b2c3d"}
	got, err := p.Upgrade(context.Background(), in, 17)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if got.DSN != in.DSN || got.ID != in.ID {
		t.Fatalf("Upgrade = %+v, want the same clone %+v (in place, no output)", got, in)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.WorkDir, "upgraded-"+in.ID))
	if err != nil {
		t.Fatalf("the program did not record its run: %v", err)
	}
	if string(raw) != in.ID+" 17" {
		t.Fatalf("program saw %q, want SAGE_CLONE_ID and SAGE_TARGET_MAJOR %q", raw,
			in.ID+" 17")
	}
}

func TestCommandUpgrade_PrintedDSNReplacesTheClones(t *testing.T) {
	p := newCommand(t, upgradeConfig(t, "upgrade-dsn"))
	got, err := p.Upgrade(context.Background(), Clone{DSN: "postgres://old@h:1/app",
		ID: "clone-a"}, 17)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if got.DSN != testCloneDSN || got.ID != "clone-a" {
		t.Fatalf("Upgrade = %+v, want DSN %q under the same id", got, testCloneDSN)
	}
}

func TestCommandUpgrade_PrintedSourceDSNIsRefused(t *testing.T) {
	p := newCommand(t, upgradeConfig(t, "upgrade-dsn-arg", testSourceDSN))
	_, err := p.Upgrade(context.Background(), Clone{DSN: testCloneDSN, ID: "clone-a"}, 17)
	if !errors.Is(err, ErrCommandDSNIsSource) {
		t.Fatalf("Upgrade printing the source = %v, want ErrCommandDSNIsSource", err)
	}
}

func TestCommandUpgrade_NotConfiguredIsUnsupported(t *testing.T) {
	p := newCommand(t, commandConfig(t, "ok"))
	_, err := p.Upgrade(context.Background(), Clone{DSN: testCloneDSN, ID: "clone-a"}, 17)
	if !errors.Is(err, ErrCommandNoUpgrade) {
		t.Fatalf("Upgrade without upgrade_argv = %v, want ErrCommandNoUpgrade", err)
	}
}

func TestCommandUpgrade_InvalidInputs(t *testing.T) {
	p := newCommand(t, upgradeConfig(t, "upgrade-ok"))
	ctx := context.Background()
	if _, err := p.Upgrade(ctx, Clone{DSN: testCloneDSN, ID: ""}, 17); !errors.Is(err,
		ErrCommandMissingField) {
		t.Errorf("empty clone id = %v, want ErrCommandMissingField", err)
	}
	for _, major := range []int{0, -1, 1001} {
		if _, err := p.Upgrade(ctx, Clone{DSN: testCloneDSN, ID: "c"}, major); !errors.Is(err,
			ErrCommandInvalidConfig) {
			t.Errorf("major %d = %v, want ErrCommandInvalidConfig", major, err)
		}
	}
	cfg := upgradeConfig(t, "upgrade-ok")
	cfg.UpgradeArgv = []string{" "}
	if _, err := NewCommandProvider(cfg); !errors.Is(err, ErrCommandEmptyProgram) {
		t.Errorf("blank upgrade program = %v, want ErrCommandEmptyProgram", err)
	}
}

func TestCommandUpgrade_FailureQuotesTheRedactedStderr(t *testing.T) {
	p := newCommand(t, upgradeConfig(t, "fail"))
	_, err := p.Upgrade(context.Background(), Clone{DSN: testCloneDSN, ID: "clone-a"}, 17)
	if !errors.Is(err, ErrCommandFailed) {
		t.Fatalf("failing program = %v, want ErrCommandFailed", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "repo locked") {
		t.Fatalf("error %q does not quote the stderr tail", err)
	}
}

func TestCommandUpgrade_GarbageOutputIsRefused(t *testing.T) {
	p := newCommand(t, upgradeConfig(t, "garbage"))
	_, err := p.Upgrade(context.Background(), Clone{DSN: testCloneDSN, ID: "clone-a"}, 17)
	if !errors.Is(err, ErrCommandBadOutput) {
		t.Fatalf("garbage output = %v, want ErrCommandBadOutput", err)
	}
}
