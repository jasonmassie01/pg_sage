package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const skipLine = `{"Action":"skip","Package":"github.com/pg-sage/sidecar/internal/rca",` +
	`"Test":"TestTier2Live_RealGemini"}` + "\n"

func writeAllow(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "allow")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunExitCodes(t *testing.T) {
	listed := writeAllow(t, "internal/rca TestTier2Live_.* # live LLM\n")
	empty := writeAllow(t, "# nothing allowed\n")
	broken := writeAllow(t, "internal/rca TestX\n")
	cases := []struct {
		name, input, want string
		args              []string
		code              int
	}{
		{"allowed", skipLine, "1 allowed, 0 unlisted", []string{"-allow", listed}, 0},
		{"unlisted", skipLine, "UNLISTED internal/rca", []string{"-allow", empty}, 1},
		{"no skips", "", "0 skipped", []string{"-allow", empty}, 0},
		{"missing file", skipLine, "open allow-list", []string{"-allow", "nope"}, 2},
		{"bad rule", skipLine, "line 1", []string{"-allow", broken}, 2},
		{"bad flag", skipLine, "flag provided but not defined", []string{"-x"}, 2},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, strings.NewReader(tc.input), &stdout, &stderr)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d (stdout=%q stderr=%q)",
				tc.name, code, tc.code, stdout.String(), stderr.String())
		}
		if out := stdout.String() + stderr.String(); !strings.Contains(out, tc.want) {
			t.Errorf("%s: output %q lacks %q", tc.name, out, tc.want)
		}
	}
}
