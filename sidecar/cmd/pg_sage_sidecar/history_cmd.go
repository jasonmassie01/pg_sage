package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/schema"
)

// pg_sage_sidecar history migrate|status: moves one database's telemetry
// history (snapshots and the query store) between its monitored database
// and the meta database (history.store), and shows where a move stands.
// The copy only reads the source; --cleanup removes the source rows, and
// only once every row is copied. It is idempotent and resumable: run it
// again after an interruption, or to copy rows written since.

const historyUsage = `usage: pg_sage_sidecar history migrate [--to meta|monitored]
           (--database-id N | --database NAME) [--batch N] [--cleanup]
       pg_sage_sidecar history status [--to meta|monitored]
           (--database-id N | --database NAME)
The monitored database's DSN comes from SAGE_HISTORY_MONITORED_DSN (or
--monitored-dsn) and the meta database's from SAGE_META_DB (or --meta-dsn).
Stop pg_sage (or leave it in its current history.store) while migrating,
then switch history.store and start it again.`

type historyArgs struct {
	sub, to         string
	id              int
	name            string
	batch           int
	cleanup         bool
	monDSN, metaDSN string
}

func parseHistoryArgs(args []string, getenv func(string) string) (historyArgs, error) {
	if len(args) == 0 || (args[0] != "migrate" && args[0] != "status") {
		return historyArgs{}, errors.New("a subcommand is required: migrate or status")
	}
	a := historyArgs{sub: args[0]}
	fs := flag.NewFlagSet("history "+a.sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.to, "to", "meta", "where the history goes: meta or monitored")
	fs.IntVar(&a.id, "database-id", 0, "the database's meta-db record id")
	fs.StringVar(&a.name, "database", "", "the database's name in the meta database")
	fs.IntVar(&a.batch, "batch", histstore.DefaultBatchRows, "rows per transaction")
	fs.BoolVar(&a.cleanup, "cleanup", false, "remove the source rows once all are copied")
	fs.StringVar(&a.monDSN, "monitored-dsn", getenv("SAGE_HISTORY_MONITORED_DSN"), "")
	fs.StringVar(&a.metaDSN, "meta-dsn", getenv("SAGE_META_DB"), "")
	if err := fs.Parse(args[1:]); err != nil {
		return a, err
	}
	return a, a.validate(fs.NArg())
}

func (a historyArgs) validate(stray int) error {
	switch {
	case stray > 0:
		return errors.New("unexpected arguments")
	case a.id == 0 && a.name == "":
		return errors.New("name the database: --database-id or --database")
	case a.id != 0 && a.name != "":
		return errors.New("--database-id or --database, not both")
	case a.id < 0:
		return errors.New("--database-id must be a positive meta-db record id")
	case a.to != "meta" && a.to != "monitored":
		return fmt.Errorf("--to must be meta or monitored, got %q", a.to)
	case a.batch < 0:
		return errors.New("--batch must be >= 0 (0 uses the default)")
	case a.monDSN == "":
		return errors.New("the monitored database's DSN is required: set " +
			"SAGE_HISTORY_MONITORED_DSN or --monitored-dsn")
	case a.metaDSN == "":
		return errors.New("the meta database's DSN is required: set SAGE_META_DB or " +
			"--meta-dsn")
	}
	return nil
}

func runHistoryCommand(ctx context.Context, args []string, getenv func(string) string,
	stdout, stderr io.Writer) int {
	a, err := parseHistoryArgs(args, getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "history: %v\n%s\n", err, historyUsage)
		return 2
	}
	if err := runHistory(ctx, a, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "history %s: %v\n", a.sub, err)
		return 1
	}
	return 0
}

func runHistory(ctx context.Context, a historyArgs, stdout, stderr io.Writer) error {
	mon, err := pgxpool.New(ctx, a.monDSN)
	if err != nil {
		return fmt.Errorf("monitored database: %w", err)
	}
	defer mon.Close()
	meta, err := pgxpool.New(ctx, a.metaDSN)
	if err != nil {
		return fmt.Errorf("meta database: %w", err)
	}
	defer meta.Close()
	if a.sub == "migrate" {
		if err := schema.BootstrapHistoryStore(ctx, meta); err != nil {
			return fmt.Errorf("make the meta database a history store: %w", err)
		}
	}
	id, err := historyDatabaseID(ctx, meta, a)
	if err != nil {
		return err
	}
	store, err := histstore.NewMeta(meta, id)
	if err != nil {
		return err
	}
	src, dst := histstore.NewMonitored(mon), store
	if a.to == "monitored" {
		src, dst = store, histstore.NewMonitored(mon)
	}
	if a.sub == "status" {
		return printHistoryStatus(ctx, src, dst, stdout)
	}
	return migrateHistory(ctx, a, src, dst, stdout, stderr)
}

// historyDatabaseID is --database-id, or --database looked up in the meta
// database's sage.databases.
func historyDatabaseID(ctx context.Context, meta *pgxpool.Pool, a historyArgs) (int,
	error) {
	if a.id > 0 {
		return a.id, nil
	}
	var id int
	err := meta.QueryRow(ctx, `SELECT id FROM sage.databases WHERE name = $1`, a.name).
		Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("database %q is not in the meta database's sage.databases",
			a.name)
	}
	if err != nil {
		return 0, fmt.Errorf("look up database %q: %w", a.name, err)
	}
	return id, nil
}

func migrateHistory(ctx context.Context, a historyArgs, src, dst histstore.Store,
	stdout, stderr io.Writer) error {
	rep, err := histstore.Migrate(ctx, src, dst, histstore.MigrateOptions{BatchRows: a.batch,
		Log: func(format string, args ...any) {
			_, _ = fmt.Fprintf(stderr, format+"\n", args...)
		}})
	if err != nil {
		return err
	}
	for _, t := range rep.Tables {
		state := "complete"
		if !t.Complete {
			state = "not complete: run it again"
		}
		_, _ = fmt.Fprintf(stdout, "sage.%s: copied %d, skipped %d unreadable, %s\n",
			t.Table, t.Copied, t.Skipped, state)
	}
	if !a.cleanup {
		return nil
	}
	clean, err := histstore.Cleanup(ctx, src, dst)
	if err != nil {
		return err
	}
	for _, table := range []string{"snapshots", "query_store"} {
		_, _ = fmt.Fprintf(stdout, "sage.%s: removed %d rows from the source\n", table,
			clean.Removed[table])
	}
	return nil
}

func printHistoryStatus(ctx context.Context, src, dst histstore.Store, stdout io.Writer) error {
	st, err := histstore.Status(ctx, src, dst)
	if err != nil {
		return err
	}
	for _, t := range st {
		state := "complete"
		switch {
		case t.Pending:
			state = "rows not copied yet"
		case !t.Complete:
			state = "interrupted: run migrate again"
		}
		_, _ = fmt.Fprintf(stdout, "sage.%s: copied %d, skipped %d unreadable, %s\n",
			t.Table, t.Copied, t.Skipped, state)
	}
	return nil
}
