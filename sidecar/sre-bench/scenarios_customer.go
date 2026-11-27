package srebench

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Customer programs (benches-12): a game day's fixtures create their own
// bench_* tables, so they say nothing about the customer's database. A
// customer program exercises the customer's own table on the clone, the
// one its binding names (internal/gameday picks the monitored database's
// hottest table). It locks the table and reads and writes no row, so the
// work does not depend on the table's size, and it never alters, drops or
// writes the table. Its report names the program and the binding's hash,
// never the table.

// skipNotInClone is the skip of a binding whose table the clone lacks (a
// clone older than the table, or a different database).
const skipNotInClone = "relation not in clone"

// customerScenarios are the customer programs of bindings, in binding
// order. A skip binding is a program that skips with the binding's reason.
func customerScenarios(bindings []gameday.Binding) ([]Scenario, error) {
	seen := map[string]bool{}
	var out []Scenario
	for _, b := range bindings {
		if seen[b.Program] {
			return nil, fmt.Errorf("game day: two bindings of customer program %q", b.Program)
		}
		seen[b.Program] = true
		switch b.Program {
		case gameday.ProgramCustomerHotTable:
			out = append(out, Scenario{ID: b.Program, Family: sre.TriggerLock,
				Class: ClassPositive, Gold: Gold{Root: "ddl_lock_queue"},
				Subject: "customer table " + b.Hash(), Program: customerHotTable(b)})
		default:
			return nil, fmt.Errorf("game day: unknown customer program %q", b.Program)
		}
	}
	return out, nil
}

// customerHotTable: a session holds ROW EXCLUSIVE on the customer table
// (the lock every writer takes) while it sleeps; a SHARE ROW EXCLUSIVE
// request (DDL strength, such as CREATE TRIGGER) queues behind it, and two
// ROW EXCLUSIVE requests queue behind that one: the DDL lock queue on the
// customer's hottest table, with the holder as its root blocker. Every
// waiter has a lock_timeout and rolls back; no row is read or written.
func customerHotTable(b gameday.Binding) program {
	ident := b.Ident()
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := bindingInClone(ctx, e, b); err != nil {
				return err
			}
			pid, err := e.lockHolder(ctx, "bench_holder", customerHolderSQL(ident),
				"SELECT pg_sleep(60); ROLLBACK")
			if err != nil {
				return holderError(b, err)
			}
			return e.sleeping(ctx, pid)
		},
		manifest: func(ctx context.Context, e *Env) error {
			for i, mode := range []string{"SHARE ROW EXCLUSIVE", "ROW EXCLUSIVE",
				"ROW EXCLUSIVE"} {
				if _, err := e.background(ctx, "bench_waiter",
					customerWaiterSQL(ident, mode)); err != nil {
					return err
				}
				if err := e.atLeastWaiters(ctx, i+1); err != nil {
					return err
				}
			}
			return nil
		},
		recover: recoverCustomer,
	}
}

// customerHolderSQL takes the holder's lock (synchronously, so a refusal
// is the inject's error); ident is the binding's quoted identifier.
func customerHolderSQL(ident string) string {
	return "SET lock_timeout = '5s'; BEGIN; " +
		"LOCK TABLE " + ident + " IN ROW EXCLUSIVE MODE"
}

// customerWaiterSQL queues one lock request of mode behind the holder.
func customerWaiterSQL(ident, mode string) string {
	return "SET lock_timeout = '90s'; BEGIN; " +
		"LOCK TABLE " + ident + " IN " + mode + " MODE; ROLLBACK"
}

// bindingInClone checks the bound table exists on the clone as an
// ordinary table before anything locks it.
func bindingInClone(ctx context.Context, e *Env, b gameday.Binding) error {
	if !b.Bound() {
		reason := b.Skip
		if reason == "" {
			reason = "the customer program has no bound table"
		}
		return &Unsupported{Reason: reason}
	}
	var kinds []string
	rows, err := e.Pool.Query(ctx, `SELECT c.relkind::text FROM pg_catalog.pg_class c
		WHERE c.oid = to_regclass($1)`, b.Ident())
	if err != nil {
		return fmt.Errorf("look up the bound table %s on the clone: %w", b.Hash(), err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return fmt.Errorf("look up the bound table %s on the clone: %w", b.Hash(), err)
		}
		kinds = append(kinds, k)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("look up the bound table %s on the clone: %w", b.Hash(), err)
	}
	switch {
	case len(kinds) == 0:
		return &Unsupported{Reason: skipNotInClone}
	case kinds[0] != "r":
		return &Unsupported{Reason: "the bound relation is not an ordinary table on the clone"}
	}
	return nil
}

// holderError is the holder's refusal without the table's name: the
// SQLSTATE of a server error, else the error with the name replaced by
// the binding's hash.
func holderError(b gameday.Binding, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return fmt.Errorf("holder lock on the bound table %s: SQLSTATE %s", b.Hash(), pg.Code)
	}
	msg := err.Error()
	for _, name := range []string{b.Ident(), b.Table, b.Schema} {
		if name != "" {
			msg = strings.ReplaceAll(msg, name, b.Hash())
		}
	}
	return fmt.Errorf("holder lock on the bound table %s: %s", b.Hash(), msg)
}

// recoverCustomer ends the program's sessions (only bench_* ones: the
// holder and the waiters), waits until they are gone and checks no lock
// wait is left. pg_terminate_backend only signals, so a terminated session
// stays in pg_stat_activity until its backend exits. The table is left as
// it was.
func recoverCustomer(ctx context.Context, e *Env) error {
	if _, err := e.Pool.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid)
		FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		  AND application_name LIKE 'bench\_%'`); err != nil {
		return fmt.Errorf("end the customer program's sessions: %w", err)
	}
	err := waitFor(ctx, "the customer program's sessions to end", func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND application_name LIKE 'bench\_%'`)
		return n == 0, err
	})
	if err != nil {
		return err
	}
	return noWaiters(ctx, e)
}

// lockHolder opens a tracked session, runs lockSQL on it and waits for it
// (an error is the lock's refusal), then runs restSQL in the background.
func (e *Env) lockHolder(ctx context.Context, app, lockSQL, restSQL string) (int, error) {
	s, err := e.connect(ctx, app)
	if err != nil {
		return 0, err
	}
	if _, err := s.conn.PgConn().Exec(ctx, lockSQL).ReadAll(); err != nil {
		close(s.done)
		return 0, err
	}
	go func() {
		defer close(s.done)
		_, s.err = s.conn.PgConn().Exec(context.Background(), restSQL).ReadAll()
	}()
	return s.pid, nil
}
