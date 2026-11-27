package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/doctor"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// "pg_sage doctor" (ux-04a) runs the read-only preflight against one
// database and prints each check with its fix. Exit 0 when nothing
// failed, 1 when a check failed, 2 on a usage error. The DSN is never
// printed and every password is masked.

const doctorSubcommand = "doctor"

const (
	doctorExitUsage = 2
	// doctorConnectTimeout applies when the DSN sets no connect_timeout.
	doctorConnectTimeout = 10 * time.Second
)

// doctorSessionParams keep every doctor session read-only and bounded.
var doctorSessionParams = map[string]string{
	"application_name":              "pg_sage_doctor",
	"statement_timeout":             "5000",
	"lock_timeout":                  "1000",
	"default_transaction_read_only": "on",
}

type doctorOptions struct {
	pgURL, configPath string
	json, sendTest    bool
}

func runDoctorCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseDoctorFlags(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "doctor: %s\n", doctor.Mask(err.Error(), nil))
		return doctorExitUsage
	}
	in, cleanup, err := doctorInputs(ctx, opts, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "doctor: %s\n", doctor.Mask(err.Error(), in.Secrets))
		return doctorExitUsage
	}
	defer cleanup()
	report := doctor.Run(ctx, in)
	if opts.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			_, _ = fmt.Fprintf(stderr, "doctor: write the report: %v\n", err)
			return 1
		}
	} else {
		writeDoctorText(stdout, report)
	}
	return report.ExitCode()
}

func parseDoctorFlags(args []string, stderr io.Writer) (doctorOptions, error) {
	var o doctorOptions
	fs := flag.NewFlagSet(doctorSubcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.pgURL, "pg-url", "", "the database to check (URL or key=value DSN)")
	fs.StringVar(&o.configPath, "config", "", "sidecar config (llm:, meta_db, alerting:)")
	fs.BoolVar(&o.json, "json", false, "print the report as JSON")
	fs.BoolVar(&o.sendTest, "send-test", false,
		"send one test notification through each configured channel")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.pgURL == "" && o.configPath == "" {
		return o, errors.New("give --pg-url (or --config with a postgres: block)")
	}
	return o, nil
}

// doctorInputs opens the target (one read-only connection), and from
// --config the model client, the meta database and the alert channels.
func doctorInputs(ctx context.Context, o doctorOptions,
	stderr io.Writer) (doctor.Inputs, func(), error) {
	in := doctor.Inputs{ExpectedSteps: schema.MigrationStepNames()}
	var cfg *config.Config
	if o.configPath != "" {
		loaded, err := loadDoctorConfig(o)
		if err != nil {
			return in, nil, err
		}
		cfg = loaded
	}
	dsn := o.pgURL
	if dsn == "" {
		dsn = cfg.Postgres.DSN()
	}
	target, secret, err := openDoctorPool(ctx, dsn)
	if err != nil {
		return in, nil, fmt.Errorf("parse --pg-url: not a PostgreSQL URL or key=value "+
			"connection string (%w)", err)
	}
	in.Target = target
	in.Secrets = append(in.Secrets, secret)
	cleanup := func() { target.Close() }
	if cfg != nil {
		in.Secrets = append(in.Secrets, doctorConfigSecrets(cfg)...)
		cleanup = withDoctorConfig(ctx, &in, cfg, o.sendTest, doctorLog(stderr, in.Secrets),
			cleanup)
	} else if o.sendTest {
		in.Notify = func(context.Context) (int, error) { return 0, nil }
	}
	return in, cleanup, nil
}

func loadDoctorConfig(o doctorOptions) (*config.Config, error) {
	args := []string{"--config", o.configPath}
	if o.pgURL != "" {
		args = append(args, "--pg-url", o.pgURL)
	}
	cfg, err := config.Load(args)
	if err != nil {
		return nil, fmt.Errorf("load --config %s: %w", o.configPath, err)
	}
	return cfg, nil
}

// openDoctorPool parses dsn and returns a lazy one-connection pool with
// pg_sage's tag and pins and read-only, bounded session settings, and the
// DSN's password.
func openDoctorPool(ctx context.Context, dsn string) (*pgxpool.Pool, string, error) {
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, "", errors.New("the string does not parse")
	}
	pc.MaxConns = 1
	pc.MinConns = 0
	if pc.ConnConfig.ConnectTimeout == 0 {
		pc.ConnConfig.ConnectTimeout = doctorConnectTimeout
	}
	// The sidecar's own session (tag and pins), so doctor sees what it
	// sees; doctor's session parameters, its application_name among them,
	// are set after it.
	selfmonitor.ConfigurePool(pc)
	for k, v := range doctorSessionParams {
		pc.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, "", fmt.Errorf("open the pool: %w", err)
	}
	return pool, pc.ConnConfig.Password, nil
}

// withDoctorConfig adds what --config provides: the model, the meta
// database and, with --send-test, the alert channels.
func withDoctorConfig(ctx context.Context, in *doctor.Inputs, cfg *config.Config,
	sendTest bool, logFn func(string, string, ...any), cleanup func()) func() {
	if client := llm.New(&cfg.LLM, logFn); client.IsEnabled() {
		in.Model = client.Probe
	}
	if sendTest {
		in.Notify = doctorAlertSender(cfg, logFn)
	}
	if !cfg.HasMetaDB() {
		return cleanup
	}
	meta, secret, err := openDoctorPool(ctx, cfg.MetaDB)
	if err != nil {
		in.MetaErr = fmt.Errorf("meta_db: %w", err)
		return cleanup
	}
	in.Meta = meta
	in.Secrets = append(in.Secrets, secret)
	return func() { meta.Close(); cleanup() }
}
