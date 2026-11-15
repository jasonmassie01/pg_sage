package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// pg_sage_sidecar verifier-backtest (lab-02) replays placebo steps over one
// database's stored history and stores the verifier's false-positive rate
// in sage.verifier_backtest, the row verify.method auto reads. It only
// reads the history; the row is its one write. The database is one the
// configuration registers (databases[].name, or the standalone database).
// With history.store: meta it refuses: it would read the monitored
// database's own, stale or empty, history (the weekly run reads the meta
// database's).

const verifierBacktestSubcommand = "verifier-backtest"

const verifierBacktestUsage = `usage: pg_sage_sidecar verifier-backtest --database NAME
           [--config FILE] [--days 90] [--placebos 300] [--seed N] [--json]
NAME is a database the configuration registers. The run is bounded by
verify.backtest_max_runtime_minutes and stores nothing when it fails or is
cut short. Exit codes: 0 stored (whatever the gate says), 1 the run failed,
2 usage.`

type backtestArgs struct {
	database, configPath string
	days, placebos       int
	seed                 int64
	asJSON               bool
}

func parseBacktestArgs(args []string) (backtestArgs, error) {
	a := backtestArgs{}
	fs := flag.NewFlagSet(verifierBacktestSubcommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.database, "database", "", "the registered database to back-test")
	fs.StringVar(&a.configPath, "config", "", "the sidecar configuration file")
	fs.IntVar(&a.days, "days", backtest.DefaultDays, "days of history to replay")
	fs.IntVar(&a.placebos, "placebos", backtest.DefaultPlacebos, "placebo steps to draw")
	fs.Int64Var(&a.seed, "seed", 1, "the placebo draw's seed")
	fs.BoolVar(&a.asJSON, "json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	switch {
	case fs.NArg() > 0:
		return a, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	case a.database == "":
		return a, errors.New("--database is required: name the database to back-test")
	case a.days <= 0:
		return a, fmt.Errorf("--days must be positive, got %d", a.days)
	case a.placebos <= 0:
		return a, fmt.Errorf("--placebos must be positive, got %d", a.placebos)
	}
	return a, nil
}

// backtestDeps are what the command opens: the configuration and a pool.
type backtestDeps struct {
	load func(path string) (*config.Config, error)
	open func(ctx context.Context, dsn string) (*pgxpool.Pool, error)
	now  func() time.Time
}

func defaultBacktestDeps() backtestDeps {
	return backtestDeps{
		load: func(path string) (*config.Config, error) {
			if path == "" {
				return config.Load(nil)
			}
			return config.Load([]string{"--config", path})
		},
		open: databricksauth.NewPool, now: time.Now}
}

func runVerifierBacktestCommand(ctx context.Context, args []string, stdout,
	stderr io.Writer) int {
	return runVerifierBacktest(ctx, args, defaultBacktestDeps(), stdout, stderr)
}

func runVerifierBacktest(ctx context.Context, args []string, deps backtestDeps, stdout,
	stderr io.Writer) int {
	a, err := parseBacktestArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verifier-backtest: %v\n%s\n", err, verifierBacktestUsage)
		return 2
	}
	cfg, err := deps.load(a.configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verifier-backtest: load the configuration: %v\n", err)
		return 2
	}
	if cfg.HistoryInMeta() {
		_, _ = fmt.Fprintf(stderr, "verifier-backtest: history.store is meta: this "+
			"command reads only the monitored database's history; the sidecar's weekly "+
			"run back-tests %q from the meta database\n", a.database)
		return 2
	}
	db, err := backtestDatabase(cfg, a.database)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verifier-backtest: %v\n", err)
		return 2
	}
	r, err := backtestOne(ctx, deps, cfg, db, a)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "verifier-backtest %s: %v\n", a.database, err)
		return 1
	}
	if err := printBacktest(stdout, r, a.asJSON); err != nil {
		_, _ = fmt.Fprintf(stderr, "verifier-backtest: %v\n", err)
		return 1
	}
	return 0
}

// backtestDatabase is the registered database named name.
func backtestDatabase(cfg *config.Config, name string) (config.DatabaseConfig, error) {
	names := make([]string, 0, len(cfg.Databases))
	for _, d := range cfg.Databases {
		if d.Name == name {
			return d, nil
		}
		names = append(names, d.Name)
	}
	slices.Sort(names)
	return config.DatabaseConfig{}, fmt.Errorf("--database %q is not a registered "+
		"database (registered: %s)", name, strings.Join(names, ", "))
}

func backtestOne(ctx context.Context, deps backtestDeps, cfg *config.Config,
	db config.DatabaseConfig, a backtestArgs) (backtest.Result, error) {
	pool, err := deps.open(ctx, db.ConnString())
	if err != nil {
		return backtest.Result{}, fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	return backtest.RunAndSave(ctx, pool, backtest.NewPostgresHistory(pool),
		backtestConfig(cfg, db.Name, a.days, a.placebos, a.seed, deps.now))
}

// backtestConfig is one run's settings from the configuration.
func backtestConfig(cfg *config.Config, name string, days, placebos int, seed int64,
	now func() time.Time) backtest.Config {
	c := backtest.Config{Database: name, Days: days, Placebos: placebos, Seed: seed,
		MaxRuntime: time.Duration(cfg.Verify.BacktestMaxRuntimeMinutes) * time.Minute,
		Now:        now}
	if cfg.Collector.IntervalSeconds > 0 {
		c.Cycle = time.Duration(cfg.Collector.IntervalSeconds) * time.Second
	}
	return c
}

func printBacktest(w io.Writer, r backtest.Result, asJSON bool) error {
	if !asJSON {
		_, err := io.WriteString(w, r.Text())
		return err
	}
	passed, reason := r.Gate()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		backtest.Result
		Passed bool   `json:"gate_passed"`
		Reason string `json:"gate_reason,omitempty"`
	}{r, passed, reason})
}
