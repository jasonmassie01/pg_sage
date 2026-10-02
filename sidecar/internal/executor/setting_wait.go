package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pg_reload_conf only signals the postmaster, which re-reads the
// configuration and signals every backend; each backend applies it when
// its own SIGHUP arrives. A read right after the reload, on any pooled
// connection, can still see the old value. Post-checks of reloaded
// settings therefore poll, bounded, for the value they expect.

const (
	// reloadSettleTimeout bounds the wait for a reloaded setting.
	reloadSettleTimeout = 5 * time.Second
	// reloadSettleInterval is the pause between reads.
	reloadSettleInterval = 100 * time.Millisecond
)

// errSettingNotInEffect: the setting never showed the expected value
// within the bound.
var errSettingNotInEffect = errors.New("setting not in effect after reload")

// awaitSetting reads the setting until it equals want, for at most
// timeout (at least one read). It returns the last value read; the error
// is errSettingNotInEffect when the bound passed, the read's error, or
// the context's.
func awaitSetting(ctx context.Context, read func(context.Context) (int64, error),
	want int64, timeout, interval time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)
	for {
		got, err := read(ctx)
		if err != nil {
			return got, fmt.Errorf("read setting: %w", err)
		}
		if got == want {
			return got, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return got, errSettingNotInEffect
		}
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case <-time.After(min(interval, remaining)):
		}
	}
}

// walKeepSizeBytes reads max_slot_wal_keep_size in bytes (-1 unlimited).
func walKeepSizeBytes(pool *pgxpool.Pool) func(context.Context) (int64, error) {
	return func(ctx context.Context) (int64, error) {
		var b int64
		err := pool.QueryRow(ctx, `/* pg_sage */ SELECT
			pg_size_bytes(current_setting('max_slot_wal_keep_size'))`).Scan(&b)
		return b, err
	}
}

// verifyWALBound checks that the bound the proposal set is in effect,
// waiting for the reload to reach the backends.
func (e *Executor) verifyWALBound(ctx context.Context, sql string) error {
	want, ok, err := parseWALKeepSizeBytes(sql)
	if !ok || err != nil {
		return fmt.Errorf("WAL backstop is not effective: cannot read the requested "+
			"max_slot_wal_keep_size (%v)", err)
	}
	if want < 0 {
		return fmt.Errorf("WAL backstop is not effective: max_slot_wal_keep_size " +
			"is unlimited")
	}
	wait := e.settingWait
	if wait <= 0 {
		wait = reloadSettleTimeout
	}
	got, err := awaitSetting(ctx, walKeepSizeBytes(e.pool), want, wait,
		reloadSettleInterval)
	if err != nil {
		return fmt.Errorf("WAL backstop is not effective: max_slot_wal_keep_size is %d "+
			"bytes after the reload, want %d: %w", got, want, err)
	}
	return nil
}
