package vectorlab

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Command is the supported pg_sage vector-lab entrypoint. Exit 2 is inconclusive evidence.
func Command(ctx context.Context, args []string, dsn string, stdout, stderr io.Writer) int {
	manifest, err := commandManifest(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "set SAGE_VECTORLAB_DATABASE_URL to the intended read-only target")
		return 1
	}
	pool, err := commandPool(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer pool.Close()
	report, err := Run(ctx, pool, manifest)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		_, _ = fmt.Fprintln(stderr, "write evidence report failed; check output destination")
		return 1
	}
	if report.Recommendation == "" {
		return 2
	}
	return 0
}

func commandManifest(args []string, stderr io.Writer) (Manifest, error) {
	flags := flag.NewFlagSet("pg_sage vector-lab", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("manifest", "", "path to strict JSON workload (required)")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: pg_sage vector-lab --manifest workload.json")
		_, _ = fmt.Fprintln(stderr, "Connection: SAGE_VECTORLAB_DATABASE_URL. Read-only, bounded experiments.")
		_, _ = fmt.Fprintln(stderr, "Exit 0: candidate qualifies; 2: none qualify; 1: experiment failed.")
	}
	if err := flags.Parse(args); err != nil {
		return Manifest{}, err
	}
	if *path == "" || flags.NArg() != 0 {
		return Manifest{}, errors.New("--manifest is required")
	}
	file, err := os.Open(*path)
	if err != nil {
		return Manifest{}, errors.New("open manifest failed; check path and read permission")
	}
	defer func() { _ = file.Close() }()
	return Decode(file)
}

func commandPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid SAGE_VECTORLAB_DATABASE_URL; check connection syntax")
	}
	cfg.MaxConns, cfg.MinConns = 1, 0
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "pg_sage_vectorlab"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("initialize vector lab database connection failed")
	}
	return pool, nil
}
