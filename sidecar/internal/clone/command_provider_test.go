package clone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/restorecmd"
)

// drill-01c: the restore command provider. The "program" is this test
// binary acting as restorecmd (TestMain hands over to it first).

const testCloneDSN = "postgres://drill@clone-host:6543/app?sslmode=disable"
const testSourceDSN = "postgres://sage@prod-host:5432/app?sslmode=disable"

func commandConfig(t *testing.T, restoreMode string, args ...string) CommandConfig {
	t.Helper()
	return CommandConfig{RestoreArgv: restorecmd.Argv(restoreMode, args...),
		DestroyArgv: restorecmd.Argv("destroy-ok"), WorkDir: t.TempDir(),
		EnvAllow: []string{restorecmd.EnvDSN}, MaxDuration: time.Minute,
		SourceDSN: testSourceDSN,
		LookupEnv: func(name string) (string, bool) {
			if name == restorecmd.EnvDSN {
				return testCloneDSN, true
			}
			return os.LookupEnv(name)
		}}
}

func newCommand(t *testing.T, cfg CommandConfig) *CommandProvider {
	t.Helper()
	p, err := NewCommandProvider(cfg)
	if err != nil {
		t.Fatalf("NewCommandProvider: %v", err)
	}
	return p
}

func drillSpec(name string) CloneSpec {
	return CloneSpec{IncludeData: true, ResourceName: name}
}

func TestCommand_CreateParsesTheLine(t *testing.T) {
	p := newCommand(t, commandConfig(t, "ok"))
	before := time.Now().UTC().Add(-2 * time.Minute)
	cl, err := p.Create(context.Background(), drillSpec("sage-drill-0a1b2c3d"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cl.DSN != testCloneDSN {
		t.Errorf("DSN = %q, want %q", cl.DSN, testCloneDSN)
	}
	if cl.ID != "clone-sage-drill-0a1b2c3d" {
		t.Errorf("ID = %q, want the id built from SAGE_DRILL_ID", cl.ID)
	}
	if cl.CreatedFrom.Before(before) || cl.CreatedFrom.After(time.Now()) {
		t.Errorf("CreatedFrom = %s, want the printed recovery point", cl.CreatedFrom)
	}
}

func TestCommand_DescribesAsFullCopy(t *testing.T) {
	p := newCommand(t, commandConfig(t, "ok"))
	c, err := DescribeOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != KindFullCopy {
		t.Fatalf("kind = %s, want full_copy (clone.allow_full_copy gates it)", c.Kind)
	}
	if _, err := CheckOptIn(context.Background(), p, false); !errors.Is(err,
		ErrFullCopyNotAllowed) {
		t.Fatalf("opt-in without allow_full_copy = %v, want ErrFullCopyNotAllowed", err)
	}
}

func TestCommand_InvalidOutputsAreDistinctErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*testing.T) CommandConfig
		want error
	}{
		{"non-JSON line", func(t *testing.T) CommandConfig {
			return commandConfig(t, "garbage")
		}, ErrCommandBadOutput},
		{"missing dsn", func(t *testing.T) CommandConfig {
			return commandConfig(t, "no-dsn")
		}, ErrCommandMissingField},
		{"dsn equal to the source", func(t *testing.T) CommandConfig {
			return commandConfig(t, "dsn-arg", testSourceDSN)
		}, ErrCommandDSNIsSource},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newCommand(t, tc.cfg(t))
			cl, err := p.Create(context.Background(), drillSpec("sage-drill-11111111"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if cl != (Clone{}) {
				t.Fatalf("clone = %+v, want none", cl)
			}
			// The program exited 0 saying it restored something: a copy may
			// be running, so the receipt must stay uncertain.
			if !uncertainCreate(err) {
				t.Fatalf("err = %v: an unusable success must be create_uncertain", err)
			}
			seen[err.Error()] = true
		})
	}
	if len(seen) != len(cases) {
		t.Fatalf("errors not distinct: %v", seen)
	}
}

func TestCommand_SourceMatchIgnoresSpellingOfTheSameDatabase(t *testing.T) {
	same := "host=prod-host port=5432 dbname=app user=other sslmode=disable"
	p := newCommand(t, commandConfig(t, "dsn-arg", same))
	if _, err := p.Create(context.Background(), drillSpec("sage-drill-22222222")); !errors.Is(
		err, ErrCommandDSNIsSource) {
		t.Fatalf("err = %v, want ErrCommandDSNIsSource for the same host, port and db", err)
	}
}

func TestCommand_ConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		edit func(*CommandConfig)
		want error
	}{
		{"no restore_argv", func(c *CommandConfig) { c.RestoreArgv = nil },
			ErrCommandNotConfigured},
		{"empty program", func(c *CommandConfig) { c.RestoreArgv = []string{"", "x"} },
			ErrCommandEmptyProgram},
		{"empty destroy program", func(c *CommandConfig) { c.DestroyArgv = []string{" "} },
			ErrCommandEmptyProgram},
		{"no destroy_argv", func(c *CommandConfig) { c.DestroyArgv = nil },
			ErrCommandEmptyProgram},
		{"zero max duration", func(c *CommandConfig) { c.MaxDuration = 0 },
			ErrCommandInvalidConfig},
		{"no work dir", func(c *CommandConfig) { c.WorkDir = "" }, ErrCommandInvalidConfig},
		{"reserved variable", func(c *CommandConfig) {
			c.EnvAllow = []string{"SAGE_DATABASE_URL"}
		}, ErrCommandInvalidConfig},
		{"malformed variable", func(c *CommandConfig) { c.EnvAllow = []string{"A=B"} },
			ErrCommandInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := commandConfig(t, "ok")
			tc.edit(&cfg)
			p, err := NewCommandProvider(cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if p != nil {
				t.Fatalf("provider = %v, want nil", p)
			}
		})
	}
}

func TestCommand_NotConfiguredIsNotAFailure(t *testing.T) {
	p, err := NewCommandProvider(CommandConfig{})
	if !errors.Is(err, ErrCommandNotConfigured) || p != nil {
		t.Fatalf("empty config = %v, %v; want ErrCommandNotConfigured", p, err)
	}
	if (CommandConfig{}).Configured() {
		t.Fatal("an empty config must not be Configured")
	}
	if !(CommandConfig{RestoreArgv: []string{"/bin/restore"}}).Configured() {
		t.Fatal("a restore_argv makes the provider configured")
	}
}

func TestCommand_CreateWithoutDrillIDIsRefused(t *testing.T) {
	p := newCommand(t, commandConfig(t, "ok"))
	_, err := p.Create(context.Background(), CloneSpec{IncludeData: true})
	if !errors.Is(err, ErrCommandNoDrillID) {
		t.Fatalf("err = %v, want ErrCommandNoDrillID (an unreceipted create)", err)
	}
}

func TestCommand_FailedProgramQuotesTheRedactedStderrTail(t *testing.T) {
	p := newCommand(t, commandConfig(t, "fail"))
	_, err := p.Create(context.Background(), drillSpec("sage-drill-33333333"))
	if !errors.Is(err, ErrCommandFailed) {
		t.Fatalf("err = %v, want ErrCommandFailed", err)
	}
	msg := err.Error()
	for _, want := range []string{"status 3", "repo locked"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	for _, secret := range []string{"hunter2", "backup:"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error %q leaks %q", msg, secret)
		}
	}
	if uncertainCreate(err) {
		t.Fatalf("a program that exited non-zero reported its failure: not uncertain")
	}
}

func TestCommand_TimeoutKillsTheProgram(t *testing.T) {
	cfg := commandConfig(t, "sleep")
	cfg.MaxDuration = 700 * time.Millisecond
	p := newCommand(t, cfg)
	start := time.Now()
	_, err := p.Create(context.Background(), drillSpec("sage-drill-44444444"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline", err)
	}
	if !uncertainCreate(err) {
		t.Fatal("a killed restore must mark the receipt create_uncertain")
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("Create took %s: the program was not killed", took)
	}
	if !strings.Contains(err.Error(), "max_duration_minutes") {
		t.Errorf("error %q does not name the bound", err)
	}
}

// A program that exits 0 while a process it started still holds its
// output (pg_ctl start without -l) may have left a server running: the
// create is uncertain, never a certain failure, and the error says why.
func TestCommand_OutputHeldOpenAfterExitIsUncertain(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	p := newCommand(t, commandConfig(t, "hold-output", release))
	start := time.Now()
	cl, err := p.Create(context.Background(), drillSpec("sage-drill-66666666"))
	releaseLingerer(t, release)
	if !errors.Is(err, ErrCommandOutputHeld) {
		t.Fatalf("err = %v, want ErrCommandOutputHeld", err)
	}
	if !uncertainCreate(err) {
		t.Fatalf("err = %v: a restore that may be running must be create_uncertain", err)
	}
	if cl != (Clone{}) {
		t.Fatalf("clone = %+v, want none", cl)
	}
	if !strings.Contains(err.Error(), "pg_ctl -l") {
		t.Errorf("error %q does not say how to fix the wrapper", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("Create took %s: the held output was waited on unbounded", took)
	}
}

// releaseLingerer lets the child that held the output exit and waits for
// it, so it does not outlive the test binary (Windows locks a running exe).
func releaseLingerer(t *testing.T, release string) {
	t.Helper()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); {
		if _, err := os.Stat(release); errors.Is(err, os.ErrNotExist) {
			time.Sleep(200 * time.Millisecond) // the exit after the remove
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the child holding the output did not exit")
}

func TestCommand_StdoutBoundary(t *testing.T) {
	for _, tc := range []struct {
		bytes int
		ok    bool
	}{{CommandMaxStdout, true}, {CommandMaxStdout + 1, false}} {
		t.Run(strconv.Itoa(tc.bytes), func(t *testing.T) {
			p := newCommand(t, commandConfig(t, "pad", strconv.Itoa(tc.bytes)))
			cl, err := p.Create(context.Background(), drillSpec("sage-drill-55555555"))
			if tc.ok {
				if err != nil || cl.ID != "clone-pad" {
					t.Fatalf("%d bytes: %+v, %v; want accepted", tc.bytes, cl, err)
				}
				return
			}
			if !errors.Is(err, ErrCommandOutputTooLarge) {
				t.Fatalf("%d bytes: err = %v, want ErrCommandOutputTooLarge", tc.bytes, err)
			}
		})
	}
}

func TestCommand_MinimalEnvironmentAndWorkDir(t *testing.T) {
	t.Setenv("SAGE_TEST_SECRET_FOR_DRILL", "do-not-pass")
	t.Setenv(restorecmd.EnvMarker, "not-allowed")
	cfg := commandConfig(t, "env")
	cfg.LookupEnv = nil // the real environment
	cfg.Database = "orders"
	t.Setenv(restorecmd.EnvDSN, testCloneDSN)
	p := newCommand(t, cfg)
	if _, err := p.Create(context.Background(), drillSpec("sage-drill-66666666")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.WorkDir, "env-sage-drill-66666666.txt"))
	if err != nil {
		t.Fatalf("the program did not run in the work dir: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	gotDir, _ := filepath.EvalSymlinks(lines[0])
	wantDir, _ := filepath.EvalSymlinks(cfg.WorkDir)
	if !strings.EqualFold(gotDir, wantDir) {
		t.Errorf("work dir = %q, want %q", lines[0], cfg.WorkDir)
	}
	names := map[string]bool{}
	for _, n := range lines[1:] {
		names[strings.ToUpper(n)] = true
	}
	for _, want := range []string{"SAGE_DRILL_ID", "SAGE_DRILL_DATABASE", restorecmd.EnvDSN} {
		if !names[want] {
			t.Errorf("environment lacks %s: %v", want, lines[1:])
		}
	}
	for _, banned := range []string{"SAGE_TEST_SECRET_FOR_DRILL", restorecmd.EnvMarker,
		"SAGE_TEST_DATABASE_URL", "GOFLAGS"} {
		if names[banned] {
			t.Errorf("environment passes %s, which env_allow does not list", banned)
		}
	}
	for n := range names {
		if !commandBaseEnvName(n) && n != "SAGE_DRILL_ID" && n != "SAGE_DRILL_DATABASE" &&
			n != restorecmd.EnvDSN {
			t.Errorf("unexpected variable %s in the program's environment", n)
		}
	}
}

func TestCommand_DestroyPassesTheCloneID(t *testing.T) {
	cfg := commandConfig(t, "ok")
	p := newCommand(t, cfg)
	if err := p.Destroy(context.Background(), Clone{ID: "clone-sage-drill-77777777"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(cfg.WorkDir, "destroyed-clone-sage-drill-77777777"))
	if err != nil || string(got) != "clone-sage-drill-77777777" {
		t.Fatalf("destroy program saw %q, %v; want SAGE_CLONE_ID", got, err)
	}
	if err := p.Destroy(context.Background(), Clone{}); err == nil {
		t.Fatal("Destroy with no clone id must fail")
	}
}

func TestCommand_DestroyFailureQuotesStderr(t *testing.T) {
	cfg := commandConfig(t, "ok")
	cfg.DestroyArgv = restorecmd.Argv("destroy-fail")
	p := newCommand(t, cfg)
	err := p.Destroy(context.Background(), Clone{ID: "clone-x"})
	if !errors.Is(err, ErrCommandFailed) || !strings.Contains(err.Error(), "could not stop") {
		t.Fatalf("err = %v, want ErrCommandFailed quoting the stderr tail", err)
	}
}

func TestCommand_SnapshotAgeIsUnknown(t *testing.T) {
	p := newCommand(t, commandConfig(t, "ok"))
	age, err := p.SnapshotAge(context.Background())
	if err == nil || age != 0 {
		t.Fatalf("SnapshotAge = %s, %v; want an error (unknown until a restore runs)", age, err)
	}
}

func TestCommand_InvalidCloneIDs(t *testing.T) {
	for _, id := range []string{"", " ", strings.Repeat("a", 201), "a\nb", "a\x00b"} {
		if err := validCommandCloneID(id); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	for _, id := range []string{"clone-1", "/var/lib/drill/sage-drill-0a1b2c3d",
		strings.Repeat("a", 200)} {
		if err := validCommandCloneID(id); err != nil {
			t.Errorf("id %q refused: %v", id, err)
		}
	}
}

func TestCommandScope(t *testing.T) {
	got := CommandScope([]string{"/usr/local/bin/sage-restore", "--stanza=main"})
	if got != "command//usr/local/bin/sage-restore" {
		t.Fatalf("scope = %q", got)
	}
	long := CommandScope([]string{"/" + strings.Repeat("d", 300)})
	if len(long) > 200 || !strings.HasPrefix(long, "command/") {
		t.Fatalf("long scope = %q (%d chars), want at most 200", long, len(long))
	}
	if long == CommandScope([]string{"/" + strings.Repeat("d", 299) + "e"}) {
		t.Fatal("two long programs share a scope")
	}
}
