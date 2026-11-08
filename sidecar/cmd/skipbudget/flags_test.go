package main

import (
	"bytes"
	"strings"
	"testing"
)

const passLine = `{"Action":"pass","Package":"github.com/pg-sage/sidecar/internal/agentguard",` +
	`"Test":"TestKill_Fleet"}` + "\n"

const ioSkipLine = `{"Action":"skip","Package":"github.com/pg-sage/sidecar/internal/collector",` +
	`"Test":"TestCollectIO"}` + "\n"

type runCase struct {
	name, input string
	args        []string
	code        int
	want, never string
}

func runCases(t *testing.T, cases []runCase) {
	t.Helper()
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, strings.NewReader(tc.input), &stdout, &stderr)
		out := stdout.String() + stderr.String()
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d (output %q)", tc.name, code, tc.code, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: output %q lacks %q", tc.name, out, tc.want)
		}
		if tc.never != "" && strings.Contains(out, tc.never) {
			t.Errorf("%s: output %q must not contain %q", tc.name, out, tc.never)
		}
	}
}

func TestRunPGFlag(t *testing.T) {
	gated := writeAllow(t, "internal/collector TestCollectIO pg<16 # pg_stat_io is PG16+\n")
	plain := writeAllow(t, "internal/rca TestTier2Live_.* # live LLM\n")
	runCases(t, []runCase{
		{"gated without -pg", ioSkipLine, []string{"-allow", gated}, 2, "-pg", ""},
		{"gated -pg 15", ioSkipLine, []string{"-allow", gated, "-pg", "15"}, 0,
			"1 allowed, 0 unlisted", ""},
		{"gated -pg 16", ioSkipLine, []string{"-allow", gated, "-pg", "16"}, 1,
			"UNLISTED internal/collector TestCollectIO", ""},
		{"gated -pg 18", ioSkipLine, []string{"-allow", gated, "-pg", "18"}, 1, "pg<16", ""},
		{"plain without -pg", skipLine, []string{"-allow", plain}, 0, "1 allowed", ""},
		{"-pg not a number", skipLine, []string{"-allow", plain, "-pg", "x"}, 2,
			"invalid value", ""},
		{"-pg negative", skipLine, []string{"-allow", plain, "-pg", "-3"}, 2, "-pg", ""},
	})
}

func TestRunRequireFlag(t *testing.T) {
	allow := writeAllow(t, "# nothing allowed\n")
	req := []string{"-allow", allow, "-require", "internal/agentguard TestKill_.*"}
	skipOnly := strings.Replace(passLine, `"pass"`, `"skip"`, 1)
	runCases(t, []runCase{
		{"empty stream", "", req, 1, "MISSING  internal/agentguard TestKill_.*", ""},
		{"only skipped", skipOnly, append(req[:4:4], "-allow", writeAllow(t,
			"internal/agentguard TestKill_.* # dedicated servers\n")), 1, "MISSING", ""},
		{"one pass", passLine, req, 0, "0 unmet requirements", "MISSING"},
		{"two requirements, one unmet", passLine,
			append(req[:4:4], "-require", "internal/pgaudit TestRealPGAuditCorrelation"), 1,
			"MISSING  internal/pgaudit TestRealPGAuditCorrelation", "MISSING  internal/agentguard"},
		{"bad spec", passLine, []string{"-allow", allow, "-require", "onlyone"}, 2,
			"require", ""},
	})
}

func TestRunStaleFlag(t *testing.T) {
	dead := writeAllow(t, "internal/rca TestTier2Live_.* # live LLM\n"+
		"internal/gone TestRenamed # nothing skips this any more\n")
	base := []string{"-allow", dead}
	runCases(t, []runCase{
		{"default warns", skipLine, base, 0, "STALE    line 2: internal/gone TestRenamed", ""},
		{"warn", skipLine, append(base[:2:2], "-stale=warn"), 0, "STALE", ""},
		{"fail", skipLine, append(base[:2:2], "-stale=fail"), 1, "STALE    line 2", ""},
		{"off", skipLine, append(base[:2:2], "-stale=off"), 0, "1 allowed", "STALE"},
		{"bogus", skipLine, append(base[:2:2], "-stale=maybe"), 2, "-stale", ""},
	})
}
