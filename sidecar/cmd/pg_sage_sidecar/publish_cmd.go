package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/publish"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// pg_sage publish outcomes (pub-01) writes the public outcome bundle: what
// pg_sage changed over a window, how it was verified and why changes were
// reverted, with the verifier's placebo false-positive rate, as a signed
// static directory. It only reads, in one read-only snapshot per database.

const publishSubcommand = "publish"

// defaultPublishKeyEnv holds the Ed25519 PKCS#8 PEM key that signs the
// bundle (or names its file through the _FILE twin).
const defaultPublishKeyEnv = "SAGE_EVIDENCE_SIGNING_KEY"

const publishUsage = `usage: pg_sage publish outcomes --out DIR [--since 90d] [--dsn DSN]...
           [--signing-key-env SAGE_EVIDENCE_SIGNING_KEY]
       pg_sage publish outcomes --sample --out DIR
Reads the pg_sage tables of each --dsn (default SAGE_HISTORY_MONITORED_DSN, else
SAGE_META_DB) and writes outcomes.json, index.html and outcomes.tar.gz (the signed
pack) into DIR, which must not exist or be empty. --since is whole days (1d to
400d) or a duration such as 36h. The signing key is an Ed25519 PKCS#8 PEM in the
named env variable or its _FILE twin. --sample writes the sample bundle with the
published sample key and reads no database.
Exit codes: 0 written, 1 a read or write failed (nothing written), 2 usage.`

// Exit codes of the publish command.
const (
	publishExitOK     = 0
	publishExitFailed = 1
	publishExitUsage  = 2
)

// publishSource is one database's read snapshot.
type publishSource interface {
	ReadOutcomes(ctx context.Context, q outcomes.Query) (outcomes.Result, error)
	LatestBacktest(ctx context.Context) (backtest.Counts, bool, error)
	Close()
}

// publishDeps are the command's environment, replaced in tests.
type publishDeps struct {
	getenv       func(string) string
	lookupSecret func(string) (string, error)
	open         func(ctx context.Context, dsn string) (publishSource, error)
	now          func() time.Time
}

func defaultPublishDeps(getenv func(string) string) publishDeps {
	return publishDeps{getenv: getenv, lookupSecret: config.LookupSecretEnv,
		open: openPublishSource, now: time.Now}
}

type publishArgs struct {
	out, keyEnv string
	dsns        []string
	since       time.Duration
	sample      bool
}

func parsePublishArgs(args []string, getenv func(string) string) (publishArgs, error) {
	var a publishArgs
	if len(args) == 0 || args[0] != "outcomes" {
		return a, errors.New("a subcommand is required: outcomes")
	}
	var since string
	fs := flag.NewFlagSet("publish outcomes", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&since, "since", "90d", "the window")
	fs.StringVar(&a.out, "out", "", "the bundle's directory")
	fs.StringVar(&a.keyEnv, "signing-key-env", defaultPublishKeyEnv, "the key's variable")
	fs.BoolVar(&a.sample, "sample", false, "write the sample bundle")
	fs.Func("dsn", "a database to read (repeatable)", func(v string) error {
		a.dsns = append(a.dsns, v)
		return nil
	})
	if err := fs.Parse(args[1:]); err != nil {
		return a, err
	}
	if fs.NArg() > 0 {
		return a, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if a.out == "" {
		return a, errors.New("--out is required")
	}
	var err error
	if a.since, err = publish.ParseSince(since); err != nil {
		return a, err
	}
	if a.sample {
		if len(a.dsns) > 0 {
			return a, errors.New("--sample reads no database; drop --dsn")
		}
		return a, nil
	}
	return a, a.resolveDSNs(getenv)
}

// resolveDSNs defaults the databases from the environment. A repeated
// --dsn is refused (flag would print its value, a password included, so
// it is checked here): its changes would count twice in a public number.
func (a *publishArgs) resolveDSNs(getenv func(string) string) error {
	for i, dsn := range a.dsns {
		if slices.Contains(a.dsns[:i], dsn) {
			return errors.New("a --dsn is repeated; each database is counted once")
		}
	}
	if len(a.dsns) > 0 {
		return nil
	}
	for _, name := range []string{"SAGE_HISTORY_MONITORED_DSN", "SAGE_META_DB"} {
		if dsn := getenv(name); dsn != "" {
			a.dsns = []string{dsn}
			return nil
		}
	}
	return errors.New("a database is required: pass --dsn, or set " +
		"SAGE_HISTORY_MONITORED_DSN or SAGE_META_DB")
}

// runPublishCommand runs pg_sage publish and returns its exit code.
func runPublishCommand(ctx context.Context, args []string, deps publishDeps,
	stdout, stderr io.Writer) int {
	a, err := parsePublishArgs(args, deps.getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "publish: %v\n%s\n", err, publishUsage)
		return publishExitUsage
	}
	if err := publish.CheckOut(a.out); err != nil {
		_, _ = fmt.Fprintf(stderr, "publish: %v\n%s\n", err, publishUsage)
		return publishExitUsage
	}
	bundle, key := publish.SampleBundle(), publish.SampleKey()
	if !a.sample {
		if key, err = publishKey(a.keyEnv, deps.lookupSecret); err != nil {
			_, _ = fmt.Fprintf(stderr, "publish: %v\n%s\n", err, publishUsage)
			return publishExitUsage
		}
		if bundle, err = readBundle(ctx, a, deps); err != nil {
			_, _ = fmt.Fprintf(stderr, "publish: %v; nothing was written\n", err)
			return publishExitFailed
		}
	}
	if err := publish.Write(a.out, bundle, key); err != nil {
		_, _ = fmt.Fprintf(stderr, "publish: %v; nothing was written\n", err)
		return publishExitFailed
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s: %s; signed with key %s\n", a.out, bundle.Statement,
		evidence.KeyID(key.Public().(ed25519.PublicKey)))
	return publishExitOK
}

// publishKey loads the signing key from env (or its _FILE twin). Errors
// name the variable, never its value.
func publishKey(env string, lookup func(string) (string, error)) (ed25519.PrivateKey,
	error) {
	pemText, err := lookup(env)
	if err != nil {
		return nil, fmt.Errorf("read the signing key from %s: %w", env, err)
	}
	if pemText == "" {
		return nil, fmt.Errorf("a public bundle is always signed: set %s (or %s_FILE) to "+
			"an Ed25519 PKCS#8 PEM key, such as the key audit.evidence.signing_key_env "+
			"names", env, env)
	}
	key, err := evidence.LoadSigningKey(pemText)
	if err != nil {
		return nil, fmt.Errorf("%s does not hold a usable Ed25519 PKCS#8 PEM key: %w", env, err)
	}
	return key, nil
}

// readBundle reads every database's window and newest back-test and
// builds the bundle.
func readBundle(ctx context.Context, a publishArgs, deps publishDeps) (publish.Bundle,
	error) {
	in := publish.Input{Since: a.since, Now: deps.now().UTC(), Databases: len(a.dsns)}
	for _, dsn := range a.dsns {
		rows, bt, found, err := readOne(ctx, dsn, in, deps)
		if err != nil {
			return publish.Bundle{}, err
		}
		in.Rows = append(in.Rows, rows...)
		if found {
			in.Backtests = append(in.Backtests, bt)
		}
	}
	return publish.Build(in), nil
}

func readOne(ctx context.Context, dsn string, in publish.Input, deps publishDeps) (
	[]outcomes.Row, backtest.Counts, bool, error) {
	name := dsnDatabase(dsn)
	src, err := deps.open(ctx, dsn)
	if err != nil {
		return nil, backtest.Counts{}, false, fmt.Errorf("connect to %s: %w", name, err)
	}
	defer src.Close()
	res, err := src.ReadOutcomes(ctx, outcomes.Query{Since: in.Since, Now: in.Now,
		Limit: outcomes.MaxLimit})
	if err != nil {
		return nil, backtest.Counts{}, false, err
	}
	if res.Truncated {
		return nil, backtest.Counts{}, false, fmt.Errorf("%s holds more than %d changes "+
			"in the window; shorten --since so every change is counted", name,
			outcomes.MaxLimit)
	}
	bt, found, err := src.LatestBacktest(ctx)
	if err != nil {
		return nil, backtest.Counts{}, false, fmt.Errorf("%s: %w", name, err)
	}
	return res.Rows, bt, found, nil
}

// dsnDatabase names a DSN's database for messages; the DSN itself is
// never printed.
func dsnDatabase(dsn string) string {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database == "" {
		return "the database"
	}
	return cfg.ConnConfig.Database
}
