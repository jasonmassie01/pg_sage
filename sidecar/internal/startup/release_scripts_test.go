package startup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ci-05: the release scripts run on Linux runners; these tests run them
// with bash on fixtures (a stub binary, a CHANGELOG).
//
// No concurrent-access tests: each run gets its own temporary directory.

func ciScript(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(wave5SidecarRoot(t), "..", "scripts", "ci", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("scripts/ci/%s: %v", name, err)
	}
	return path
}

// runBash runs a bash script with args in dir and returns its combined
// output and exit code.
func runBash(t *testing.T, dir, script string, args ...string) (string, int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the release scripts are bash and bash is missing: %v", err)
	}
	cmd := exec.Command(bash, append([]string{script}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("run %s: %v", script, err)
	return "", -1
}

// stubBinary writes a bash stub that prints body for --version and exits
// with code.
func stubBinary(t *testing.T, dir, body, code string) string {
	t.Helper()
	path := filepath.Join(dir, "stub.sh")
	script := "test \"$1\" = --version || { echo \"stub called with $*\" >&2; exit 9; }\n" +
		"printf '%s' '" + body + "'\nexit " + code + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

const goodVersionLine = "pg_sage 2.6.0 (commit: abc1234, built: 2026-11-09T00:00:00Z, " +
	"sql-ast: libpg_query, go: go1.27.4)\n"

func TestSmokeVersionAcceptsTheTaggedLibpgQueryBuild(t *testing.T) {
	smoke := ciScript(t, "smoke-version.sh")
	cases := []struct{ name, want, body string }{
		{"stable tag", "2.6.0", goodVersionLine},
		{"release candidate", "2.6.0-rc.1", strings.Replace(goodVersionLine, "2.6.0",
			"2.6.0-rc.1", 1)},
		{"log line first", "2.6.0", "level=info msg=starting\n" + goodVersionLine},
	}
	for _, c := range cases {
		dir := t.TempDir()
		stub := stubBinary(t, dir, c.body, "0")
		out, code := runBash(t, dir, smoke, c.want, "bash", stub)
		if code != 0 {
			t.Errorf("%s: exit %d, want 0: %s", c.name, code, out)
			continue
		}
		if !strings.Contains(out, "pg_sage "+c.want+" (") {
			t.Errorf("%s: output %q does not print the version line", c.name, out)
		}
	}
}

func TestSmokeVersionRejectsWrongBuilds(t *testing.T) {
	smoke := ciScript(t, "smoke-version.sh")
	cases := []struct{ name, want, body, code, says string }{
		{"no libpg_query", "2.6.0", strings.Replace(goodVersionLine, "libpg_query",
			"unavailable (built without cgo)", 1), "0", "libpg_query"},
		{"rc binary for a stable tag", "2.6.0", strings.Replace(goodVersionLine, "2.6.0",
			"2.6.0-rc.1", 1), "0", "2.6.0"},
		{"longer version", "2.6.0", strings.Replace(goodVersionLine, "2.6.0", "2.6.01", 1),
			"0", "2.6.0"},
		{"dev build", "2.6.0", strings.Replace(goodVersionLine, "2.6.0", "dev", 1), "0", "dev"},
		{"binary fails", "2.6.0", goodVersionLine, "3", "exit"},
		{"no output", "2.6.0", "", "0", "no version line"},
		{"other program", "2.6.0", "pg_sage_other 2.6.0 (sql-ast: libpg_query)\n", "0",
			"no version line"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		stub := stubBinary(t, dir, c.body, c.code)
		out, code := runBash(t, dir, smoke, c.want, "bash", stub)
		if code == 0 {
			t.Errorf("%s: exit 0, want a failure: %s", c.name, out)
			continue
		}
		if !strings.Contains(out, c.says) {
			t.Errorf("%s: output %q does not say %q", c.name, out, c.says)
		}
	}
}

func TestSmokeVersionRejectsMissingArguments(t *testing.T) {
	smoke := ciScript(t, "smoke-version.sh")
	dir := t.TempDir()
	stub := stubBinary(t, dir, goodVersionLine, "0")
	for name, args := range map[string][]string{
		"nothing":       nil,
		"no command":    {"2.6.0"},
		"empty version": {"", "bash", stub},
		"v prefix":      {"v2.6.0", "bash", stub},
	} {
		out, code := runBash(t, dir, smoke, args...)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (usage): %s", name, code, out)
		}
		if !strings.Contains(out, "usage") && !strings.Contains(out, "without its v") {
			t.Errorf("%s: output %q explains nothing", name, out)
		}
	}
}
