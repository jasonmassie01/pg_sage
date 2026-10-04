package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
)

// pg_sage bench export-replay --investigation ID [--keep-identifiers]
// [--out FILE] (roadmap 2.4): exports a contested investigation (an
// operator refuted it, or confirmed it with another actual root) as a
// redacted PGIncidentBench replay case, reading the control database
// named by an environment variable (PG_SAGE_EXPORT_DSN, or --dsn-env):
// a DSN never goes on the command line. Identifiers are keyed hashes
// unless --keep-identifiers; secrets and PII-like literals are removed
// either way. The API serves the same export
// (GET /api/v1/databases/{db}/investigations/{id}/replay-case).

const (
	exportDSNEnv     = "PG_SAGE_EXPORT_DSN"
	exportReplayHelp = "usage: pg_sage bench export-replay --investigation ID " +
		"[--keep-identifiers] [--out FILE] [--dsn-env NAME] [--timeout 30s]\n" +
		"  reads the control database from $" + exportDSNEnv + " (or --dsn-env)"
)

type exportArgs struct {
	id, out, dsnEnv string
	keep            bool
	timeout         time.Duration
}

func parseExportArgs(args []string, stderr io.Writer) (exportArgs, bool) {
	var a exportArgs
	fs := flag.NewFlagSet("bench export-replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&a.id, "investigation", "", "the contested investigation's id")
	fs.BoolVar(&a.keep, "keep-identifiers", false, "keep identifiers (opt-in)")
	fs.StringVar(&a.out, "out", "", "write the case to this file (default: stdout)")
	fs.StringVar(&a.dsnEnv, "dsn-env", exportDSNEnv, "environment variable with the DSN")
	fs.DurationVar(&a.timeout, "timeout", 30*time.Second, "time limit")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || a.id == "" || a.timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, exportReplayHelp)
		return a, false
	}
	return a, true
}

// runExportReplay runs the subcommand; 2 is a usage error, 1 a failure.
func runExportReplay(ctx context.Context, args []string, getenv func(string) string,
	stdout, stderr io.Writer) int {
	a, ok := parseExportArgs(args, stderr)
	if !ok {
		return 2
	}
	dsn := getenv(a.dsnEnv)
	if dsn == "" {
		_, _ = fmt.Fprintf(stderr, "set %s to the control database URL (%s)\n", a.dsnEnv,
			exportReplayHelp)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	exp, err := exportReplayCase(ctx, dsn, sre.UUID(a.id), a.keep)
	if err != nil {
		// Errors never carry the DSN's credentials (redacted like evidence).
		_, _ = fmt.Fprintf(stderr, "bench export-replay: %s\n", sre.RedactText(err.Error()))
		return 1
	}
	raw, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bench export-replay: encode: %v\n", err)
		return 1
	}
	raw = append(raw, '\n')
	if a.out == "" {
		_, _ = stdout.Write(raw)
		return 0
	}
	if err := os.WriteFile(a.out, raw, 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "bench export-replay: write %s: %v\n", a.out, err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "wrote %s (%s); %s\n", a.out, exp.Case.ID, exp.Promote)
	return 0
}

func exportReplayCase(ctx context.Context, dsn string, id sre.UUID,
	keep bool) (sre.ReplayCaseExport, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return sre.ReplayCaseExport{}, fmt.Errorf("connect to the control database: %w", err)
	}
	defer pool.Close()
	store, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		return sre.ReplayCaseExport{}, err
	}
	scope, err := store.LookupScope(ctx, id)
	if err != nil {
		return sre.ReplayCaseExport{}, err
	}
	return sre.ExportReplayCaseFromStore(ctx, store, scope, id,
		sre.ReplayExportOptions{KeepIdentifiers: keep})
}
