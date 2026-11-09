package srebench

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// The pre-run sweep (benches-03). A bench process that stopped mid-run (a
// crash, kill -9) skipped its programs' Recover and left cluster-wide
// fixtures: an ALTER SYSTEM setting (the next checkpoint programs then
// skip: "already set by ALTER SYSTEM"), an immediately-reserving slot that
// retains WAL without limit, or a prepared transaction that pins the xmin
// horizon. OpenEnv removes them before any program runs, under the WAL
// cluster lock every slot and ALTER SYSTEM program holds for its whole
// run, so no live program's fixture is swept. Only bench objects go:
// settings a bench recorded in sage.bench_fixture_state, inactive slots
// named bench_slot_, and prepared transactions in the bench namespaces
// older than leftoverAge (prepared transactions are made outside the WAL
// lock, so a younger one may be a live run's).

// leftoverAge is the longest a run lives (gameday.MaxRunTime): a bench
// prepared transaction older than this is a crashed run's.
const leftoverAge = 6 * time.Hour

// benchSlotNamespace starts every bench slot's name (slotPrefix).
const benchSlotNamespace = "bench_slot_"

// benchGID is a prepared-transaction name a bench made; anything else is
// never rolled back (and is quoted only once it matches).
var benchGID = regexp.MustCompile(`^bench_(gid|prepared)_[0-9A-Za-z_]+$`)

// settingName is a recorded setting the sweep may reset.
var settingName = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)

// sweepLeftovers removes what crashed bench runs left on e's cluster,
// waiting for the WAL cluster lock first.
func sweepLeftovers(ctx context.Context, e *Env, preparedAge time.Duration) error {
	release, err := lockCluster(ctx, e.DSN)
	if err != nil {
		return fmt.Errorf("sweep a crashed run's leftovers: take the WAL lock: %w", err)
	}
	defer release()
	return errors.Join(resetRecordedSettings(ctx, e), dropBenchSlots(ctx, e),
		rollbackBenchPrepared(ctx, e, preparedAge))
}

// queryNames lists the single text column of sql.
func (e *Env) queryNames(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := e.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// resetRecordedSettings resets each setting a bench recorded and forgets it.
func resetRecordedSettings(ctx context.Context, e *Env) error {
	names, err := e.queryNames(ctx, "SELECT setting FROM sage.bench_fixture_state")
	if err != nil {
		return fmt.Errorf("sweep: read sage.bench_fixture_state: %w", err)
	}
	var errs []error
	for _, name := range names {
		if !settingName.MatchString(name) {
			errs = append(errs, fmt.Errorf("sweep: recorded setting %q is not a name", name))
			continue
		}
		if err := e.resetSystem(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("sweep: reset %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// dropBenchSlots drops the inactive bench slots this database can drop:
// physical ones, and logical ones of this database (a logical slot is
// dropped from its own database; another database's run sweeps it).
func dropBenchSlots(ctx context.Context, e *Env) error {
	slots, err := e.queryNames(ctx, `SELECT slot_name::text
		FROM pg_catalog.pg_replication_slots
		WHERE starts_with(slot_name::text, $1) AND NOT active
		  AND (slot_type = 'physical' OR database = current_database())`,
		benchSlotNamespace)
	if err != nil {
		return fmt.Errorf("sweep: list bench slots: %w", err)
	}
	var errs []error
	for _, slot := range slots {
		if _, err := e.Pool.Exec(ctx, "SELECT pg_drop_replication_slot($1)", slot); err != nil {
			errs = append(errs, fmt.Errorf("sweep: drop slot %s: %w", slot, err))
		}
	}
	return errors.Join(errs...)
}

// rollbackBenchPrepared rolls back this database's bench prepared
// transactions prepared more than age ago.
func rollbackBenchPrepared(ctx context.Context, e *Env, age time.Duration) error {
	gids, err := e.queryNames(ctx, `SELECT gid FROM pg_catalog.pg_prepared_xacts
		WHERE database = current_database()
		  AND (starts_with(gid, $1) OR starts_with(gid, $2))
		  AND prepared < now() - make_interval(secs => $3)`,
		benchGIDNamespace, legacyGIDNamespace, age.Seconds())
	if err != nil {
		return fmt.Errorf("sweep: list bench prepared transactions: %w", err)
	}
	var errs []error
	for _, gid := range gids {
		if !benchGID.MatchString(gid) {
			continue // a name a bench never makes: not ours to resolve
		}
		// ROLLBACK PREPARED takes no parameters; gid matched benchGID, so
		// it holds no quote.
		if _, err := e.Pool.Exec(ctx, "ROLLBACK PREPARED '"+gid+"'"); err != nil {
			errs = append(errs, fmt.Errorf("sweep: roll back %s: %w", gid, err))
		}
	}
	return errors.Join(errs...)
}
