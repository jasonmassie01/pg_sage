package benchingest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	srebench "github.com/pg-sage/sidecar/sre-bench"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// The CI artifact pgincidentbench-pg17 holds one bench report per shard
// ({core,reactive,runway}/pgincidentbench.json, schema
// pg_sage.pgincidentbench.v1) and, at its top, the replay corpus report
// (pgincidentbench-replay.json: corpus_schema, no schema). Pointing
// bench_results_path at the artifact must ingest the three bench reports
// and pass over the replay report without an error (an error is an
// hourly WARN); a malformed file is still an error. A replay report
// named on its own is an error that says what it is.
//
// No concurrent-access test here: Ingest keeps no state of its own.

// parsingLedger validates every report the way the earned ledger does
// (earned.ParseBenchReport) before storing it.
type parsingLedger struct{ fakeLedger }

func (l *parsingLedger) IngestBench(ctx context.Context, raw []byte,
	in earned.BenchIngest) (earned.EvalRun, error) {
	if _, err := earned.ParseBenchReport(raw, time.Now()); err != nil {
		return earned.EvalRun{}, err
	}
	return l.fakeLedger.IngestBench(ctx, raw, in)
}

func benchReport(family string) srebench.Report {
	rate := 1.0
	return srebench.Report{Schema: srebench.ReportSchema,
		GeneratedAt: time.Now().UTC().Add(-time.Hour), Gated: []string{"causal-graph"},
		Cells: []srebench.CellRecord{{Arm: "causal-graph", Family: family, Runs: 12,
			SafePass: srebench.Metric{K: 12, N: 12}, Top1: srebench.Metric{K: 11, N: 12},
			MechanismPrecision: &rate}}}
}

func replayReport() srebench.ReplayReport {
	return srebench.ReplayReport{Schema: replay.Schema,
		GeneratedAt: time.Now().UTC().Add(-time.Hour), LLM: srebench.LLMConfig{Mode: "off"},
		Cases: 3, Corpus: []srebench.CorpusCount{{Family: "lock_blocking",
			Class: "positive", N: 3}}, Arms: []string{"causal-graph"},
		Gated: []string{"causal-graph"}}
}

// ciArtifact lays out pgincidentbench-pg17: three shard reports and the
// replay report at the top. It returns the root and the replay report.
func ciArtifact(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	for shard, family := range map[string]string{"core": "lock_blocking",
		"reactive": "checkpoint_storm", "runway": "wal_retention"} {
		if _, _, err := srebench.WriteReport(filepath.Join(root, shard),
			benchReport(family)); err != nil {
			t.Fatal(err)
		}
	}
	replayPath, _, err := srebench.WriteReplayReport(root, replayReport())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(replayPath) != "pgincidentbench-replay.json" {
		t.Fatalf("replay report written as %s", replayPath)
	}
	return root, replayPath
}

func TestIngestCIArtifactSkipsTheReplayReport(t *testing.T) {
	root, replayPath := ciArtifact(t)
	// A bundle next to the replay report is not even looked at.
	write(t, replayPath+BundleSuffix, "valid:not the replay report")
	ledger, verifier := &parsingLedger{}, &fakeVerifier{}
	res, err := Ingest(context.Background(), ledger, verifier,
		Source{Path: root, Actor: "bench_results_path"})
	if err != nil {
		t.Fatalf("ingest of the CI artifact: %v (an hourly WARN)", err)
	}
	if res.Files != 3 || res.Added != 3 || res.Signed != 0 || verifier.calls != 0 {
		t.Fatalf("result = %+v, verifier calls %d; want 3 bench reports", res,
			verifier.calls)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != replayPath {
		t.Fatalf("skipped = %v, want [%s]", res.Skipped, replayPath)
	}
	if len(ledger.calls) != 3 {
		t.Fatalf("%d reports reached the ledger, want 3", len(ledger.calls))
	}
	for _, c := range ledger.calls {
		if strings.Contains(string(c.raw), `"corpus_schema"`) {
			t.Fatalf("the replay report reached the ledger: %.80s", c.raw)
		}
	}
	// The hourly re-ingest: duplicates, the replay report skipped again.
	res, err = Ingest(context.Background(), ledger, verifier,
		Source{Path: root, Actor: "bench_results_path"})
	if err != nil || res.Added != 0 || res.Files != 3 || len(res.Skipped) != 1 {
		t.Fatalf("re-ingest = %+v (%v)", res, err)
	}
}

// A shipped directory gets the same treatment: the release could carry
// the replay report next to the shard reports.
func TestIngestShippedDirectorySkipsTheReplayReport(t *testing.T) {
	root, replayPath := ciArtifact(t)
	ledger := &parsingLedger{}
	res, err := Ingest(context.Background(), ledger, nil,
		Source{Path: root, Shipped: true, Actor: "release_bench"})
	if err != nil || res.Added != 3 || len(res.Skipped) != 1 ||
		res.Skipped[0] != replayPath {
		t.Fatalf("shipped ingest = %+v (%v)", res, err)
	}
}

func TestIngestDirectoryStillReportsAMalformedFile(t *testing.T) {
	root, _ := ciArtifact(t)
	broken := filepath.Join(root, "broken.json")
	write(t, broken, `{"schema": "pg_sage.pgincidentbench.v1", "cells": [`)
	ledger := &parsingLedger{}
	res, err := Ingest(context.Background(), ledger, nil,
		Source{Path: root, Actor: "bench_results_path"})
	if !errors.Is(err, earned.ErrInvalidReport) {
		t.Fatalf("err = %v, want ErrInvalidReport for the malformed file", err)
	}
	if errors.Is(err, ErrReplayReport) || !strings.Contains(err.Error(), "broken.json") ||
		strings.Contains(err.Error(), "pgincidentbench-replay.json") {
		t.Fatalf("the error must name only the malformed file: %v", err)
	}
	if res.Added != 3 || len(res.Skipped) != 1 {
		t.Fatalf("result = %+v: the bench reports must still go in", res)
	}
}

// Files that are not replay reports are never skipped: no schema at all,
// an object that is not JSON, a JSON array, an empty file.
func TestIngestDirectoryReportsFilesThatAreNeitherKind(t *testing.T) {
	for name, body := range map[string]string{
		"no-schema.json": `{"generated_at": "2026-10-01T00:00:00Z", "cells": []}`,
		"not-json.json":  `corpus_schema: pg_sage.sre.replay_case.v1`,
		"array.json":     `[{"corpus_schema": "pg_sage.sre.replay_case.v1"}]`,
		"empty.json":     ``,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, name), body)
			res, err := Ingest(context.Background(), &parsingLedger{}, nil,
				Source{Path: dir, Actor: "bench_results_path"})
			if !errors.Is(err, earned.ErrInvalidReport) || errors.Is(err, ErrReplayReport) {
				t.Fatalf("err = %v, want ErrInvalidReport", err)
			}
			if len(res.Skipped) != 0 || res.Files != 1 || res.Added != 0 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

// A bench report carries the replay section nested ("replay":
// {"corpus_schema": ...}); only a top-level corpus_schema without a
// schema makes a replay report.
func TestIngestBenchReportWithANestedReplaySection(t *testing.T) {
	dir := t.TempDir()
	rep := benchReport("lock_blocking")
	section := replayReport()
	rep.Replay = &section
	if _, _, err := srebench.WriteReport(dir, rep); err != nil {
		t.Fatal(err)
	}
	ledger := &parsingLedger{}
	res, err := Ingest(context.Background(), ledger, nil,
		Source{Path: dir, Actor: "bench_results_path"})
	if err != nil || res.Added != 1 || len(res.Skipped) != 0 {
		t.Fatalf("bench report with a replay section = %+v (%v)", res, err)
	}
	// Both keys at the top: a bench report (its schema decides), not skipped.
	both := filepath.Join(t.TempDir(), "both.json")
	write(t, both, `{"schema": "pg_sage.pgincidentbench.v0", "corpus_schema": "x"}`)
	res, err = Ingest(context.Background(), ledger, nil,
		Source{Path: filepath.Dir(both), Actor: "bench_results_path"})
	if !errors.Is(err, earned.ErrInvalidReport) || len(res.Skipped) != 0 {
		t.Fatalf("schema and corpus_schema = %+v (%v), want the schema refusal", res, err)
	}
}

func TestIngestExplicitReplayFileIsAClearError(t *testing.T) {
	_, replayPath := ciArtifact(t)
	ledger := &parsingLedger{}
	res, err := Ingest(context.Background(), ledger, &fakeVerifier{},
		Source{Path: replayPath, Actor: "bench_results_path"})
	if !errors.Is(err, ErrReplayReport) {
		t.Fatalf("err = %v, want ErrReplayReport", err)
	}
	msg := err.Error()
	for _, want := range []string{replayPath, "replay-corpus report", "not a bench report",
		replay.Schema, ReportName} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, `schema ""`) {
		t.Errorf("error is the generic schema refusal: %q", msg)
	}
	if len(ledger.calls) != 0 || res.Added != 0 || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v, %d ledger calls", res, len(ledger.calls))
	}
}
