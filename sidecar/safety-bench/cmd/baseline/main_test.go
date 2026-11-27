package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	safetybench "github.com/pg-sage/sidecar/safety-bench"
)

// No concurrent-access tests: a one-shot command over local files.

const pg17 = "PostgreSQL 17.10 (Debian 17.10-1.pgdg12+1) on x86_64-pc-linux-gnu"

func report(parser bool, version string) safetybench.Report {
	return safetybench.Report{
		ServerVersion: version,
		SQLParser:     parser,
		Designs:       []string{"read_only_txn", "privilege_role"},
		ReadOnly: []safetybench.CaseResult{{ID: "RO-14", Attempts: []safetybench.Attempt{
			{Design: "read_only_txn", Observed: safetybench.ClassReadOnly, ChecksumsIntact: true},
			{Design: "privilege_role", Observed: safetybench.ClassPrivilege,
				ChecksumsIntact: true},
		}}},
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCmd(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestWritesBaselineAndKeepsOverrides(t *testing.T) {
	dir := t.TempDir()
	rep, out := filepath.Join(dir, "agentsafetybench.json"), filepath.Join(dir, "baseline.json")
	writeJSON(t, rep, report(true, pg17))
	keep := safetybench.PGOverride{Major: 14, Design: "read_only_txn", Case: "RO-14",
		Observed: safetybench.ClassExecuted, Reason: "lo_import before 16"}
	writeJSON(t, out, safetybench.Baseline{
		Designs: map[string]map[string]safetybench.RefusalClass{
			"read_only_txn": {"RO-14": safetybench.ClassExecuted}},
		PGOverrides: []safetybench.PGOverride{keep},
	})
	code, stdout, stderr := runCmd("-report", rep, "-out", out)
	if code != 0 || !strings.Contains(stdout, out) {
		t.Fatalf("run = %d, stdout %q, stderr %q; want 0 naming %s", code, stdout, stderr, out)
	}
	b, err := safetybench.LoadBaseline(out)
	if err != nil {
		t.Fatal(err)
	}
	if b.Designs["read_only_txn"]["RO-14"] != safetybench.ClassReadOnly ||
		b.Designs["privilege_role"]["RO-14"] != safetybench.ClassPrivilege {
		t.Fatalf("cells = %v, want the report's observed classes", b.Designs)
	}
	if len(b.PGOverrides) != 1 || b.PGOverrides[0] != keep {
		t.Fatalf("overrides = %+v, want %+v kept", b.PGOverrides, keep)
	}
}

func TestWritesAFreshBaseline(t *testing.T) {
	dir := t.TempDir()
	rep, out := filepath.Join(dir, "r.json"), filepath.Join(dir, "sub", "baseline.json")
	writeJSON(t, rep, report(true, pg17))
	if code, _, stderr := runCmd("-report", rep, "-out", out); code != 0 {
		t.Fatalf("run = %d, stderr %q", code, stderr)
	}
	b, err := safetybench.LoadBaseline(out)
	if err != nil || len(b.Designs) != 2 || len(b.PGOverrides) != 0 {
		t.Fatalf("baseline = %+v, %v; want two designs and no overrides", b, err)
	}
}

func TestRefusals(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	writeJSON(t, good, report(true, pg17))
	noParser := filepath.Join(dir, "nocgo.json")
	writeJSON(t, noParser, report(false, pg17))
	pg14 := filepath.Join(dir, "pg14.json")
	writeJSON(t, pg14, report(true, "PostgreSQL 14.23 (Debian)"))
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	badOut := filepath.Join(dir, "bad-baseline.json")
	if err := os.WriteFile(badOut, []byte(`{"designs": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no report flag", nil, 2, "-report"},
		{"unknown flag", []string{"-nope"}, 2, "nope"},
		{"missing report", []string{"-report", filepath.Join(dir, "none.json")}, 1, "none.json"},
		{"malformed report", []string{"-report", broken, "-out", filepath.Join(dir, "o.json")},
			1, broken},
		{"no parser", []string{"-report", noParser, "-out", filepath.Join(dir, "o.json")},
			1, "parser"},
		{"not the reference major", []string{"-report", pg14, "-out",
			filepath.Join(dir, "o.json")}, 1, "pg_overrides"},
		{"unreadable existing baseline", []string{"-report", good, "-out", badOut}, 1, badOut},
	}
	for _, tc := range cases {
		code, _, stderr := runCmd(tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: code %d stderr %q; want %d mentioning %q", tc.name, code, stderr,
				tc.code, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "o.json")); !os.IsNotExist(err) {
		t.Errorf("a refused run wrote o.json (stat err %v)", err)
	}
	if raw, _ := os.ReadFile(badOut); string(raw) != `{"designs": 1}` {
		t.Errorf("a refused run rewrote the existing baseline: %q", raw)
	}
}
