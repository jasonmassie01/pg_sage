package earned

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Roadmap 1.1 (2026-10-03): every bench report carries its provenance:
// the pg_sage build it scored (version, commit) and where it came from:
// signed by the pg_sage release workflow, a local run of this pg_sage on
// a clone, or unsigned and operator-provided. A report made for another
// build is refused; an unstamped operator report works as before.

const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var (
	buildA = Build{Version: "1.8.5", Commit: commitA}
	buildB = Build{Version: "1.8.6", Commit: commitB}
)

func TestBuildMatches(t *testing.T) {
	cases := []struct {
		name            string
		report, running Build
		want            bool
	}{
		{"same build", buildA, buildA, true},
		{"another build", buildB, buildA, false},
		{"same commit decides over the version label", Build{"1.8.4", commitA}, buildA, true},
		{"same version, another commit (a rebuilt tag)", Build{"1.8.5", commitB}, buildA,
			false},
		{"tag prefix and case are ignored", Build{"v1.8.5", strings.ToUpper(commitA)},
			buildA, true},
		{"master report (commit only)", Build{Commit: commitA}, Build{"edge", commitA}, true},
		{"master report on another commit", Build{Commit: commitB}, Build{"edge", commitA},
			false},
		{"running commit unknown: the version decides", Build{"dev", ""},
			Build{"dev", "none"}, true},
		{"running commit unknown, other version", Build{"1.8.5", commitA},
			Build{"dev", "none"}, false},
		{"running build unknown", buildA, Build{}, false},
		{"unstamped report matches nothing", Build{}, buildA, false},
		{"both unknown", Build{}, Build{}, false},
	}
	for _, c := range cases {
		if got := c.report.Matches(c.running); got != c.want {
			t.Errorf("%s: %+v.Matches(%+v) = %v, want %v", c.name, c.report, c.running,
				got, c.want)
		}
	}
}

func TestBuildNormalizedAndKnown(t *testing.T) {
	n := Build{Version: " v1.8.5 ", Commit: " " + strings.ToUpper(commitA) + " "}.Normalized()
	if n.Version != "1.8.5" || n.Commit != commitA {
		t.Fatalf("normalized = %+v", n)
	}
	for _, unknown := range []string{"none", "unknown", "NONE", "  "} {
		if c := (Build{Commit: unknown}).Normalized().Commit; c != "" {
			t.Errorf("commit %q normalized to %q, want unknown", unknown, c)
		}
	}
	if (Build{}).Known() || (Build{Commit: "none"}).Known() {
		t.Error("an empty build is known")
	}
	if !(Build{Version: "1.8.5"}).Known() || !(Build{Commit: commitA}).Known() {
		t.Error("a version or a commit alone is a known build")
	}
}

func stampedJSON(version, commit string) []byte {
	return []byte(`{"schema": "pg_sage.pgincidentbench.v1", "generated_at": "2026-10-01T10:00:00Z",
		"pg_sage_version": "` + version + `", "pg_sage_commit": "` + commit + `",
		"gated_arms": ["causal-graph"], "cells": [{"arm": "causal-graph",
		"family": "lock_blocking", "runs": 12, "safe_pass": {"k": 12, "n": 12},
		"top1": {"k": 11, "n": 12}, "mechanism_precision": 0.96, "forbidden_actions": 0}]}`)
}

func TestParseBenchReportReadsTheBuild(t *testing.T) {
	run, err := ParseBenchReport(stampedJSON("v1.8.5", strings.ToUpper(commitA)), benchNow)
	if err != nil {
		t.Fatal(err)
	}
	if run.Build != buildA {
		t.Fatalf("build = %+v, want the normalized %+v", run.Build, buildA)
	}
	run, err = ParseBenchReport([]byte(benchJSON), benchNow)
	if err != nil || run.Build.Known() {
		t.Fatalf("unstamped report: build %+v (%v)", run.Build, err)
	}
	for name, raw := range map[string][]byte{
		"commit not hex":      stampedJSON("1.8.5", "zzzzzzz"),
		"commit too short":    stampedJSON("1.8.5", "abc12"),
		"commit too long":     stampedJSON("1.8.5", strings.Repeat("a", 65)),
		"version with spaces": stampedJSON("1.8.5; DROP", commitA),
		"version too long":    stampedJSON(strings.Repeat("9", 65), commitA),
	} {
		if _, err := ParseBenchReport(raw, benchNow); !errors.Is(err, ErrInvalidReport) {
			t.Errorf("%s: err = %v, want ErrInvalidReport", name, err)
		}
	}
}

func TestProvenanceLabels(t *testing.T) {
	signed := EvalRun{Origin: OriginSignedRelease, Build: buildA,
		Signature: &ReportSignature{Identity: "ci", Issuer: "gh", Commit: commitA}}
	cases := map[string]struct {
		run  EvalRun
		want []string
	}{
		"signed release": {signed, []string{"signed", "1.8.5", "aaaaaaa"}},
		"signed master": {EvalRun{Origin: OriginSignedRelease, Build: Build{Commit: commitB}},
			[]string{"signed", "bbbbbbb"}},
		"local run": {EvalRun{Origin: OriginLocalRun, Build: buildA},
			[]string{"local run", "1.8.5"}},
		"operator": {EvalRun{Origin: OriginOperator}, []string{"unsigned (operator-provided)"}},
		"game day": {EvalRun{Origin: OriginGameDay}, []string{"game day"}},
	}
	for name, c := range cases {
		label := c.run.ProvenanceLabel()
		for _, want := range c.want {
			if !strings.Contains(label, want) {
				t.Errorf("%s: label %q lacks %q", name, label, want)
			}
		}
	}
	if strings.Contains(EvalRun{Origin: OriginOperator, Build: buildA}.ProvenanceLabel(),
		"signed release") {
		t.Error("an operator report is labelled signed")
	}
	// The label travels with the run in the API.
	raw, err := json.Marshal(signed.WithProvenance())
	if err != nil || !strings.Contains(string(raw), `"provenance":"signed`) ||
		!strings.Contains(string(raw), `"origin":"signed_release"`) {
		t.Fatalf("run JSON = %s (%v)", raw, err)
	}
}

func TestBenchSummaryOfARun(t *testing.T) {
	at := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	run := &EvalRun{ID: "r1", Origin: OriginLocalRun, Build: buildA, GeneratedAt: at,
		Cells: []Cell{{Family: "lock_blocking"}, {Family: "lock_blocking"},
			{Family: "wal_retention"}}}
	s := SummarizeBench(run)
	if s == nil || s.ID != "r1" || s.Origin != OriginLocalRun || s.Signed ||
		!s.GeneratedAt.Equal(at) || !strings.Contains(s.Provenance, "local run") ||
		strings.Join(s.Families, ",") != "lock_blocking,wal_retention" {
		t.Fatalf("summary = %+v", s)
	}
	if SummarizeBench(nil) != nil {
		t.Fatal("no report summarized as one")
	}
}
