package restorecmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// T6 test round 1: the fake restore program's own tests (covfloor's 50%
// helper floor). Each mode runs in process with stdout and stderr
// captured; the clone and drill tests run it as a real program.
// No concurrent-access test: the tests swap the process's stdout, so they
// run one at a time, and the program keeps no shared state.

// capture runs mode and returns its exit code, stdout and stderr.
func capture(t *testing.T, mode string, args ...string) (int, string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := run(mode, args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = outW.Close()
	_ = errW.Close()
	out, _ := io.ReadAll(outR)
	serr, _ := io.ReadAll(errR)
	return code, string(out), string(serr)
}

func line(t *testing.T, out string) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("output %q is not one JSON line: %v", out, err)
	}
	return m
}

func TestArgvAndMainOutsideTheProgram(t *testing.T) {
	argv := Argv("ok", "a")
	if len(argv) != 4 || argv[1] != Marker || argv[2] != "ok" || argv[3] != "a" {
		t.Fatalf("Argv = %v", argv)
	}
	if Main() {
		t.Fatal("Main acted as the program in a normal test run")
	}
}

func TestResultModesPrintAValidLine(t *testing.T) {
	t.Setenv(EnvDSN, "postgres://clone/db")
	t.Setenv(envDrillID, "d7")
	code, out, _ := capture(t, "ok")
	if m := line(t, out); code != 0 || m["dsn"] != "postgres://clone/db" ||
		m["id"] != "clone-d7" || m["recovery_point"] == "" {
		t.Fatalf("ok = %d %v", code, m)
	}
	if code, out, _ = capture(t, "drill-id"); code != 0 || line(t, out)["id"] != "d7" {
		t.Fatalf("drill-id = %d %q", code, out)
	}
	if code, out, _ = capture(t, "no-dsn"); code != 0 || line(t, out)["dsn"] != "" {
		t.Fatalf("no-dsn = %d %q", code, out)
	}
	if code, out, _ = capture(t, "dsn-arg", "postgres://src"); code != 0 ||
		line(t, out)["dsn"] != "postgres://src" {
		t.Fatalf("dsn-arg = %d %q", code, out)
	}
	if code, out, _ = capture(t, "upgrade-dsn"); code != 0 ||
		line(t, out)["dsn"] != "postgres://clone/db" {
		t.Fatalf("upgrade-dsn = %d %q", code, out)
	}
	if code, out, _ = capture(t, "upgrade-dsn-arg"); code != 0 || line(t, out)["dsn"] != "" {
		t.Fatalf("upgrade-dsn-arg without an argument = %d %q", code, out)
	}
	if code, out, _ = capture(t, "garbage"); code != 0 || !strings.Contains(out, "OK") {
		t.Fatalf("garbage = %d %q", code, out)
	}
}

func TestPadPrintsExactlyTheByteCount(t *testing.T) {
	t.Setenv(EnvDSN, "postgres://clone/db")
	code, out, _ := capture(t, "pad", "500")
	if code != 0 || len(out) != 500 || line(t, out)["id"] != "clone-pad" {
		t.Fatalf("pad 500 = %d, %d bytes", code, len(out))
	}
	for _, arg := range []string{"x", "3"} {
		if code, out, serr := capture(t, "pad", arg); code != 2 || out != "" || serr == "" {
			t.Fatalf("pad %q = %d %q %q, want exit 2 with a reason", arg, code, out, serr)
		}
	}
}

func TestFailureModesExitNonZeroWithAReason(t *testing.T) {
	for mode, want := range map[string]int{"fail": 3, "destroy-fail": 1, "no-such": 2} {
		code, out, serr := capture(t, mode)
		if code != want || out != "" || serr == "" {
			t.Errorf("%s = %d %q %q, want exit %d and stderr", mode, code, out, serr, want)
		}
	}
}

func TestDestroyAndUpgradeRecordTheirIDs(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, _, serr := capture(t, "destroy-ok"); code != 4 || serr == "" {
		t.Fatalf("destroy-ok without an id = %d %q", code, serr)
	}
	if code, _, _ := capture(t, "upgrade-ok"); code != 4 {
		t.Fatalf("upgrade-ok without an id = %d", code)
	}
	t.Setenv(envCloneID, "a/b:c")
	t.Setenv(envMajor, "18")
	if code, _, _ := capture(t, "destroy-ok"); code != 0 {
		t.Fatalf("destroy-ok = %d", code)
	}
	if b, err := os.ReadFile("destroyed-a_b_c"); err != nil || string(b) != "a/b:c" {
		t.Fatalf("destroy record = %q, %v", b, err)
	}
	t.Setenv(envCloneID, "c1")
	if code, _, _ := capture(t, "upgrade-ok"); code != 0 {
		t.Fatalf("upgrade-ok = %d", code)
	}
	if b, err := os.ReadFile("upgraded-c1"); err != nil || string(b) != "c1 18" {
		t.Fatalf("upgrade record = %q, %v", b, err)
	}
}

func TestEnvWritesTheNamesItWasGiven(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv(envDrillID, "d9")
	t.Setenv(EnvMarker, "canary")
	code, out, _ := capture(t, "env")
	if code != 0 || line(t, out)["id"] != "clone-d9" {
		t.Fatalf("env = %d %q", code, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "env-d9.txt"))
	if err != nil || !strings.Contains(string(b), EnvMarker) {
		t.Fatalf("env file = %q, %v", b, err)
	}
}

func TestLingerEndsWhenItsReleaseExists(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if code := linger(release); code != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("linger = %d after %v", code, time.Since(start))
	}
	if _, err := os.Stat(release); !os.IsNotExist(err) {
		t.Fatalf("release file left behind: %v", err)
	}
	if firstArg(nil) != "" || firstArg([]string{"a", "b"}) != "a" {
		t.Fatal("firstArg")
	}
}
