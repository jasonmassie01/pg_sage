package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// guard-01b: docs/backfill-pressure.md ships a psql loop and a GitHub
// Actions step that call the CLI between batches. Every
// `pg_sage_sidecar pressure` command in it must parse with the CLI's own
// flag parser, the workflow must be valid YAML that runs it, and the
// psql loop's \if blocks must balance.

const pressureDocPath = "../../../docs/backfill-pressure.md"

func readPressureDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(pressureDocPath))
	if err != nil {
		t.Fatalf("read %s: %v", pressureDocPath, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

var fencePattern = regexp.MustCompile("(?ms)^```([a-z]*)\n(.*?)^```")

// fencedBlocks maps each fence language to its blocks, in order.
func fencedBlocks(doc string) map[string][]string {
	out := map[string][]string{}
	for _, m := range fencePattern.FindAllStringSubmatch(doc, -1) {
		out[m[1]] = append(out[m[1]], m[2])
	}
	return out
}

// pressureInvocations are the argument lists of every CLI call in text:
// shell variables read as "1" (a valid database, table, job or batch), and
// the call ends at a shell or psql separator.
func pressureInvocations(text string) [][]string {
	var calls [][]string
	const cmd = "pg_sage_sidecar pressure"
	for _, line := range strings.Split(text, "\n") {
		_, rest, ok := strings.Cut(line, cmd)
		if !ok {
			continue
		}
		var args []string
		for _, tok := range strings.Fields(rest) {
			if strings.ContainsAny(tok, ";|)`&>") || tok == "\\" {
				break
			}
			tok = strings.Trim(tok, `"'`)
			if strings.HasPrefix(tok, "$") {
				tok = "1"
			}
			args = append(args, tok)
		}
		calls = append(calls, args)
	}
	return calls
}

func TestPressureCmdDocs_EveryCommandParses(t *testing.T) {
	doc := readPressureDoc(t)
	calls := pressureInvocations(doc)
	if len(calls) < 3 {
		t.Fatalf("the doc has %d pressure commands, want the shell, psql and CI examples",
			len(calls))
	}
	for _, args := range calls {
		if _, err := parsePressureArgs(args); err != nil {
			t.Errorf("documented command %q does not parse: %v", args, err)
		}
	}
	for _, code := range []string{"10", "20"} {
		if !strings.Contains(doc, code) {
			t.Errorf("the doc never names exit code %s", code)
		}
	}
}

func TestPressureCmdDocs_WorkflowIsValidYAML(t *testing.T) {
	blocks := fencedBlocks(readPressureDoc(t))["yaml"]
	if len(blocks) == 0 {
		t.Fatal("the doc has no yaml block (the GitHub Actions step)")
	}
	var wf struct {
		Jobs map[string]struct {
			Env   map[string]string `yaml:"env"`
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(blocks[0]), &wf); err != nil {
		t.Fatalf("workflow YAML: %v", err)
	}
	runs := 0
	for _, job := range wf.Jobs {
		if job.Env["SAGE_API_URL"] == "" || !strings.Contains(job.Env["SAGE_API_TOKEN"],
			"secrets.") {
			t.Errorf("the workflow job must take SAGE_API_URL and the token from secrets: %v",
				job.Env)
		}
		for _, s := range job.Steps {
			if len(pressureInvocations(s.Run)) > 0 {
				runs++
			}
		}
	}
	if runs == 0 {
		t.Fatal("no workflow step runs pg_sage_sidecar pressure")
	}
}

func TestPressureCmdDocs_PsqlLoopBalances(t *testing.T) {
	blocks := fencedBlocks(readPressureDoc(t))["psql"]
	if len(blocks) == 0 {
		t.Fatal("the doc has no psql block (the psql loop)")
	}
	loop := blocks[0]
	opens := strings.Count(loop, `\if `)
	if opens == 0 || opens != strings.Count(loop, `\endif`) {
		t.Fatalf("psql loop has %d \\if and %d \\endif", opens, strings.Count(loop, `\endif`))
	}
	if len(pressureInvocations(loop)) == 0 || !strings.Contains(loop, `\gset`) {
		t.Fatal("the psql loop does not call the CLI and read its answer with \\gset")
	}
	if !strings.Contains(readPressureDoc(t), `\watch`) {
		t.Fatal("the doc has no \\watch example")
	}
}
