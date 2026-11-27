package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/migration"
)

// pg_sage lint-migration (migration-11): the CI gate. It reviews
// one migration file with the same engine as MCP lint_migration and the
// advisor, prints JSON, GitHub annotations or SARIF, and exits 0 safe,
// 1 review, 2 risky or 3 on a tool error (unreadable file, no connection,
// a catalog read that failed), so a broken gate never reads as "review".

const lintMigrationSubcommand = "lint-migration"

// lintDSNEnv names the monitored database the gate reads catalogs from.
const lintDSNEnv = "SAGE_LINT_DATABASE_URL"

// Exit codes beyond the verdicts.
const lintExitToolError = 3

// Catalog read bounds: each read is cut off at 2 s (the endpoint's
// budget) and never waits on a lock for more than 1 s.
var lintReadTimeouts = catalogread.Timeouts{Statement: 2 * time.Second, Lock: time.Second}

const lintConnectTimeout = 10 * time.Second

const lintMigrationUsage = `usage: pg_sage lint-migration --file x.sql [--static]
           [--pg-version 14-18] [--row-threshold N] [--format json|github|sarif]
The monitored database's DSN comes from SAGE_LINT_DATABASE_URL (or
SAGE_LINT_DATABASE_URL_FILE); --static reads no database. Exit codes:
0 safe, 1 review, 2 risky, 3 tool error.`

type lintMigrationArgs struct {
	file, format, dsn string
	static            bool
	pgVersion         int
	rowThreshold      int64
}

func parseLintMigrationArgs(args []string, getenv func(string) string) (
	lintMigrationArgs, error) {
	var a lintMigrationArgs
	fs := flag.NewFlagSet(lintMigrationSubcommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.file, "file", "", "the migration file to review")
	fs.StringVar(&a.format, "format", "json", "json, github or sarif")
	fs.BoolVar(&a.static, "static", false, "read no database: intrinsic hazards only")
	fs.IntVar(&a.pgVersion, "pg-version", 0, "server major (14-18); 0 reads the server's")
	fs.Int64Var(&a.rowThreshold, "row-threshold", 0, "migration.ddl_row_threshold")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	a.dsn = getenv(lintDSNEnv)
	switch {
	case fs.NArg() > 0:
		return a, fmt.Errorf("unexpected arguments %v", fs.Args())
	case a.file == "":
		return a, errors.New("--file is required")
	case a.format != "json" && a.format != "github" && a.format != "sarif":
		return a, fmt.Errorf("--format must be json, github or sarif, got %q", a.format)
	case a.rowThreshold < 0:
		return a, errors.New("--row-threshold must be >= 0 (0 uses the default)")
	case !a.static && a.dsn == "":
		return a, errors.New("no database: set " + lintDSNEnv + " or pass --static")
	}
	return a, nil
}

// runLintMigration is the subcommand; it returns the process exit code.
func runLintMigration(ctx context.Context, args []string, getenv func(string) string,
	stdout, stderr io.Writer) int {
	a, err := parseLintMigrationArgs(args, getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lint-migration: %v\n%s\n", err, lintMigrationUsage)
		return lintExitToolError
	}
	sql, err := readMigrationFile(a.file)
	if err != nil {
		return lintToolError(stderr, err)
	}
	rep, err := reviewMigration(ctx, a, sql)
	if err != nil {
		return lintToolError(stderr, err)
	}
	if err := writeLintReport(stdout, a.format, a.file, rep); err != nil {
		return lintToolError(stderr, fmt.Errorf("write the report: %w", err))
	}
	return rep.ExitCode()
}

func lintToolError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "lint-migration: %v (exit 3: the migration was not "+
		"reviewed)\n", err)
	return lintExitToolError
}

// readMigrationFile reads at most one byte past the review limit, so an
// oversized file is refused without reading all of it.
func readMigrationFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read the migration file: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, migration.MaxReviewBytes+1))
	if err != nil {
		return "", fmt.Errorf("read the migration file %s: %w", path, err)
	}
	return string(b), nil
}

// reviewMigration connects (unless static) with one connection and
// reviews sql under the gate's catalog bounds.
func reviewMigration(ctx context.Context, a lintMigrationArgs, sql string) (
	migration.Report, error) {
	opts := migration.ReviewOptions{PGVersion: a.pgVersion, RowThreshold: a.rowThreshold,
		Static: a.static}
	if a.static {
		return migration.Review(ctx, nil, sql, opts)
	}
	pool, err := connectLintDatabase(ctx, a.dsn)
	if err != nil {
		return migration.Report{}, err
	}
	defer pool.Close()
	return migration.Review(ctx, catalogread.New(pool, lintReadTimeouts), sql, opts)
}

func connectLintDatabase(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse %s: the DSN is not valid", lintDSNEnv)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = "pg_sage lint-migration"
	cctx, cancel := context.WithTimeout(ctx, lintConnectTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(cctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", lintDSNEnv, err)
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to %s: %w", lintDSNEnv, err)
	}
	return pool, nil
}
