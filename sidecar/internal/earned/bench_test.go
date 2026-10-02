package earned

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// PGIncidentBench JSON (sidecar/sre-bench report.go, schema
// pg_sage.pgincidentbench.v1) is the benchmark evidence for promotion.
// Only per-family cells and the gated arms are kept; the per-run records
// are not needed and are dropped.

const benchJSON = `{
  "schema": "pg_sage.pgincidentbench.v1",
  "generated_at": "2026-10-01T10:00:00Z",
  "server_version": "17.2",
  "repeats": 2,
  "arms": ["causal-graph", "causal-graph+llm", "always-escalate", "rules-only"],
  "gated_arms": ["causal-graph", "causal-graph+llm"],
  "pending_arms": {"causal-graph+llm": "a live model needs a URL"},
  "cells": [
    {"arm": "causal-graph", "family": "lock_blocking", "runs": 16,
     "safe_pass": {"k": 16, "n": 16, "rate": 1}, "top1": {"k": 13, "n": 14, "rate": 0.93},
     "mechanism_precision": 0.97, "forbidden_actions": 0},
    {"arm": "causal-graph+llm", "family": "lock_blocking", "pending": "no model", "runs": 0,
     "safe_pass": {"k": 0, "n": 0, "rate": null}, "top1": {"k": 0, "n": 0, "rate": null},
     "mechanism_precision": null, "forbidden_actions": 0},
    {"arm": "rules-only", "family": "wal_retention", "runs": 10,
     "safe_pass": {"k": 7, "n": 10, "rate": 0.7}, "top1": {"k": 5, "n": 10, "rate": 0.5},
     "mechanism_precision": 0.6, "forbidden_actions": 2}
  ],
  "gates": [{"id": "top1", "family": "lock_blocking", "arm": "causal-graph", "status": "pass"}],
  "runs": [{"scenario": "lock-1", "family": "lock_blocking", "arm": "causal-graph"}]
}`

var benchNow = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

func TestParseBenchReportKeepsCellsAndGatedArms(t *testing.T) {
	run, err := ParseBenchReport([]byte(benchJSON), benchNow)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if run.Schema != BenchSchema || !run.GeneratedAt.Equal(
		time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) || len(run.Cells) != 3 {
		t.Fatalf("run = %+v", run)
	}
	if strings.Join(run.Gated, ",") != "causal-graph,causal-graph+llm" {
		t.Fatalf("gated = %v", run.Gated)
	}
	c := run.Cells[0]
	if c.Arm != "causal-graph" || c.Family != "lock_blocking" || c.Top1 != (Metric{13, 14}) ||
		c.SafePass != (Metric{16, 16}) || c.Precision == nil || *c.Precision != 0.97 ||
		c.Runs != 16 {
		t.Fatalf("cell = %+v", c)
	}
	if run.Cells[1].Pending != "no model" || run.Cells[1].Precision != nil {
		t.Fatalf("pending cell = %+v", run.Cells[1])
	}
	if run.Cells[2].Forbidden != 2 {
		t.Fatalf("forbidden = %d", run.Cells[2].Forbidden)
	}
	if len(run.SHA256) != 64 {
		t.Fatalf("sha256 = %q", run.SHA256)
	}
}

func TestParseBenchReportRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"empty":         ``,
		"not json":      `{"schema": `,
		"fenced json":   "```json\n" + benchJSON + "\n```",
		"wrong schema":  strings.Replace(benchJSON, "pgincidentbench.v1", "pgincidentbench.v9", 1),
		"no schema":     `{"generated_at": "2026-10-01T10:00:00Z", "cells": [{}]}`,
		"no time":       strings.Replace(benchJSON, `"generated_at": "2026-10-01T10:00:00Z",`, "", 1),
		"future":        strings.Replace(benchJSON, "2026-10-01T10:00:00Z", "2026-10-03T10:00:00Z", 1),
		"no cells":      `{"schema": "pg_sage.pgincidentbench.v1", "generated_at": "2026-10-01T10:00:00Z", "cells": []}`,
		"k above n":     strings.Replace(benchJSON, `{"k": 13, "n": 14`, `{"k": 15, "n": 14`, 1),
		"negative n":    strings.Replace(benchJSON, `{"k": 13, "n": 14`, `{"k": 0, "n": -1`, 1),
		"bad family":    strings.Replace(benchJSON, `"family": "lock_blocking", "runs": 16`, `"family": "DROP TABLE", "runs": 16`, 1),
		"precision > 1": strings.Replace(benchJSON, `0.97`, `1.7`, 1),
		"trailing data": benchJSON + `{"schema": "x"}`,
	}
	for name, raw := range cases {
		if _, err := ParseBenchReport([]byte(raw), benchNow); !errors.Is(err, ErrInvalidReport) {
			t.Errorf("%s: err = %v, want ErrInvalidReport", name, err)
		}
	}
	big := strings.Repeat(" ", MaxReportBytes+1)
	if _, err := ParseBenchReport([]byte(big), benchNow); !errors.Is(err, ErrInvalidReport) {
		t.Errorf("oversized report: err = %v", err)
	}
}

// The same report always hashes the same, so a repeated upload is a
// duplicate rather than new evidence.
func TestParseBenchReportHashIsStable(t *testing.T) {
	a, errA := ParseBenchReport([]byte(benchJSON), benchNow)
	b, errB := ParseBenchReport([]byte(benchJSON), benchNow.Add(time.Hour))
	if errA != nil || errB != nil || a.SHA256 != b.SHA256 {
		t.Fatalf("hashes %q %q (%v %v)", a.SHA256, b.SHA256, errA, errB)
	}
	c, _ := ParseBenchReport([]byte(strings.Replace(benchJSON, "13", "12", 1)), benchNow)
	if c.SHA256 == a.SHA256 {
		t.Fatal("different reports share a hash")
	}
}
