package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Roadmap 2.4: pg_sage bench export-replay --investigation ID exports a
// contested investigation as a redacted PGIncidentBench replay case,
// reading the control database named by an environment variable (never a
// flag, so the DSN stays out of process listings and shell history).

func runExport(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runExportReplay(context.Background(), args,
		func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestExportReplayCommand_Usage(t *testing.T) {
	for name, args := range map[string][]string{
		"no investigation": {},
		"bad flag":         {"--nope"},
		"extra argument":   {"--investigation", "x", "more"},
	} {
		if code, _, stderr := runExport(t, nil, args...); code != 2 ||
			!strings.Contains(stderr, "usage") {
			t.Errorf("%s: %d %q", name, code, stderr)
		}
	}
	var out, errOut bytes.Buffer
	if code := runBenchCommand([]string{"export-replay"}, &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "export-replay") {
		t.Errorf("bench export-replay without args = %d %q", code, errOut.String())
	}
}

func TestExportReplayCommand_NeedsTheDSNVariable(t *testing.T) {
	code, _, stderr := runExport(t, map[string]string{},
		"--investigation", string(sre.NewUUID()))
	if code != 2 || !strings.Contains(stderr, exportDSNEnv) {
		t.Fatalf("no DSN = %d %q", code, stderr)
	}
	code, _, stderr = runExport(t, map[string]string{"OTHER": "postgres://u:hunter2@h/db"},
		"--investigation", string(sre.NewUUID()), "--dsn-env", "OTHER", "--timeout", "2s")
	if code == 0 || strings.Contains(stderr, "hunter2") {
		t.Fatalf("an unreachable DSN = %d %q (the DSN must never be printed)", code, stderr)
	}
}

// contestedInvestigation runs a lock investigation on the live database
// and records the operator's refutation.
func contestedInvestigation(t *testing.T, dsn string) sre.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: twoEdgeRunner{},
		Config: sre.DefaultCoordinatorConfig(fmt.Sprintf("w3a-cli:%d", time.Now().UnixNano()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "case:w3a-cli", Kind: sre.TriggerLock,
		Subject: "incident on public.orders", IdempotencyKey: "w3a-cli"})
	if err != nil || coord.Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigate: %v", err)
	}
	if _, err := sre.NewService("orders", coord, st).RecordOutcome(ctx, inv.ID,
		sre.OutcomeRequest{Verdict: "refuted", ActualNode: "ddl_lock_queue",
			Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	return inv.ID
}

func TestExportReplayCommand_WritesAValidCase(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	id := contestedInvestigation(t, dsn)
	env := map[string]string{exportDSNEnv: dsn}
	code, stdout, stderr := runExport(t, env, "--investigation", string(id))
	if code != 0 {
		t.Fatalf("export = %d %q", code, stderr)
	}
	var exp struct {
		Case json.RawMessage `json:"case"`
	}
	if err := json.Unmarshal([]byte(stdout), &exp); err != nil {
		t.Fatalf("stdout is not the export: %v", err)
	}
	c, err := replay.Parse(exp.Case)
	if err == nil {
		err = c.Validate(probes.Catalog())
	}
	if err != nil || c.Gold.Root != "ddl_lock_queue" {
		t.Fatalf("case = %+v (%v)", c.Gold, err)
	}
	if strings.Contains(stdout, "public.orders") || strings.Contains(stdout, dsn) {
		t.Fatal("the default export must hash identifiers and never name the DSN")
	}
	out := filepath.Join(t.TempDir(), "case.json")
	code, stdout, stderr = runExport(t, env, "--investigation", string(id),
		"--keep-identifiers", "--out", out)
	if code != 0 || stdout != "" || !strings.Contains(stderr, out) {
		t.Fatalf("--out = %d %q %q", code, stdout, stderr)
	}
	raw, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(raw), "public.orders") {
		t.Fatalf("written case: %v", err)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm()&0o077 != 0 &&
		os.PathSeparator == '/' {
		t.Fatalf("the case file must be private: %v %v", info.Mode(), err)
	}
}

func TestExportReplayCommand_UnknownInvestigation(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	code, _, stderr := runExport(t, map[string]string{exportDSNEnv: dsn},
		"--investigation", string(sre.NewUUID()))
	if code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("unknown id = %d %q", code, stderr)
	}
}
