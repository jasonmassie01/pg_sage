package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/sagepool"
)

// pg_sage ledger (ux-02a): what pg_sage changed in one database over a
// window, one line per change (or the GET /api/v1/outcomes payload with
// --json). It only reads. Exit 0 for a read, 2 for a usage error or a
// database that could not be read.

const ledgerSubcommand = "ledger"

const ledgerUsage = `usage: pg_sage ledger [--since 24h] [--database NAME]
           [--verdict V] [--limit N] [--json] [--dsn DSN]
Reads the pg_sage tables of one database: --dsn, else SAGE_HISTORY_MONITORED_DSN
(the monitored database), else SAGE_META_DB. --database names it in the output
(default: the DSN's database name).`

type ledgerArgs struct {
	dsn, database string
	asJSON        bool
	query         outcomes.Query
}

func parseLedgerArgs(args []string, getenv func(string) string) (ledgerArgs, error) {
	var a ledgerArgs
	var since, verdict string
	fs := flag.NewFlagSet("ledger", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&since, "since", "24h", "the window, such as 24h or 168h")
	fs.StringVar(&a.database, "database", "", "the database's name in the output")
	fs.StringVar(&verdict, "verdict", "", "only this verdict")
	limit := fs.Int("limit", outcomes.DefaultLimit, "rows printed")
	fs.BoolVar(&a.asJSON, "json", false, "print the API payload as JSON")
	fs.StringVar(&a.dsn, "dsn", "", "the database to read")
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	if fs.NArg() > 0 {
		return a, errors.New("unexpected arguments")
	}
	q := outcomes.Query{Now: time.Now()}
	var err error
	if q.Since, err = outcomes.ParseSince(since); err != nil {
		return a, err
	}
	if q.Verdict, err = outcomes.ParseVerdict(verdict); err != nil {
		return a, err
	}
	if q.Limit, err = outcomes.ParseLimit(fmt.Sprint(*limit)); err != nil {
		return a, err
	}
	a.query = q
	return a, a.resolveDSN(getenv)
}

func (a *ledgerArgs) resolveDSN(getenv func(string) string) error {
	for _, dsn := range []string{a.dsn, getenv("SAGE_HISTORY_MONITORED_DSN"),
		getenv("SAGE_META_DB")} {
		if dsn != "" {
			a.dsn = dsn
			break
		}
	}
	if a.dsn == "" {
		return errors.New("a database is required: set SAGE_HISTORY_MONITORED_DSN or " +
			"SAGE_META_DB, or pass --dsn")
	}
	if a.database == "" {
		cfg, err := sagepool.ParseConfig(a.dsn)
		if err != nil {
			return err
		}
		a.database = cfg.ConnConfig.Database
	}
	return nil
}

func runLedgerCommand(ctx context.Context, args []string, getenv func(string) string,
	stdout, stderr io.Writer) int {
	a, err := parseLedgerArgs(args, getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger: %v\n%s\n", err, ledgerUsage)
		return 2
	}
	pool, err := sagepool.New(ctx, a.dsn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger: connect to %s: %v\n", a.database, err)
		return 2
	}
	defer pool.Close()
	res, err := outcomes.Read(ctx, pool, a.database, a.query)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ledger: %v\n", err)
		return 2
	}
	if a.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return 0
	}
	if len(res.Rows) == 0 {
		_, _ = fmt.Fprintf(stdout, "no changes in the last %s\n", res.Since)
	}
	for _, r := range res.Rows {
		_, _ = fmt.Fprintln(stdout, ledgerLine(r))
	}
	return 0
}

// ledgerLine is one change on one line.
func ledgerLine(r outcomes.Row) string {
	parts := []string{r.ExecutedAt.UTC().Format(time.RFC3339), r.Database,
		fmt.Sprintf("#%d", r.ActionID), r.What, r.Verdict}
	if r.Measured != nil {
		parts = append(parts, fmt.Sprintf("measured %+.1f%% %s", r.Measured.ChangePct,
			r.Measured.Metric))
	} else if r.MeasuredReason != "" {
		parts = append(parts, "not measured: "+r.MeasuredReason)
	}
	if r.Expected != nil {
		parts = append(parts, fmt.Sprintf("expected %+.1f%%", r.Expected.ChangePct))
	}
	parts = append(parts, "rollback "+r.RollbackState)
	if r.EvidenceID != "" {
		parts = append(parts, "evidence "+r.EvidenceID)
	}
	if r.Why != "" {
		parts = append(parts, "why: "+r.Why)
	}
	return strings.Join(strings.Fields(strings.Join(parts, "  |  ")), " ")
}
