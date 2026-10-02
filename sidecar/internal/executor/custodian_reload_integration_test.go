package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// The WAL bound's post-check against the reload race: ALTER SYSTEM plus
// pg_reload_conf only signals the postmaster, and a pooled backend may
// answer before its own SIGHUP arrives. These tests write the setting and
// delay or withhold the reload, so the race is deterministic: the old
// check read the setting once, immediately.

func walBoundProposal(size string) CustodianProposal {
	return CustodianProposal{Feature: "wal",
		SQL:           "ALTER SYSTEM SET max_slot_wal_keep_size = '" + size + "'",
		TargetObjects: []string{"slot:bench_none"}}
}

func alterKeepSize(t *testing.T, ctx context.Context, pool *pgxpool.Pool, size string,
	reload bool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "ALTER SYSTEM SET max_slot_wal_keep_size = '"+size+"'"); err != nil {
		t.Fatalf("alter system: %v", err)
	}
	if reload {
		if _, err := pool.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}
}

// The reload lands 300 ms after the post-check starts: it waits for it.
func TestVerifyWALBoundWaitsForTheReload(t *testing.T) {
	pool, ctx := requireDB(t)
	resetSlotKeep(t, pool)
	alterKeepSize(t, ctx, pool, "768MB", false)
	done := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, err := pool.Exec(context.Background(), "SELECT pg_reload_conf()")
		done <- err
	}()
	e := New(pool, config.DefaultConfig(), zeroTime(), func(string, string, ...any) {})
	criterion, err := e.verifyCustodianAction(ctx, walBoundProposal("768MB"),
		custodianBaseline{})
	if reloadErr := <-done; reloadErr != nil {
		t.Fatalf("delayed reload: %v", reloadErr)
	}
	if err != nil || criterion != "custodian_wal" {
		t.Fatalf("post-check = %q, %v; want custodian_wal verified after the reload",
			criterion, err)
	}
}

// Another bound is in effect and the requested value is never written:
// the post-check fails honestly, naming both values, within its bound.
// (Writing the requested value without a reload would race any other
// test that reloads the configuration; a value never written cannot.)
func TestVerifyWALBoundFailsWhenTheValueNeverTakesEffect(t *testing.T) {
	pool, ctx := requireDB(t)
	resetSlotKeep(t, pool)
	alterKeepSize(t, ctx, pool, "896MB", true)
	awaitVisible(t, ctx, pool, "896MB")
	e := New(pool, config.DefaultConfig(), zeroTime(), func(string, string, ...any) {})
	e.settingWait = 400 * time.Millisecond
	start := time.Now()
	criterion, err := e.verifyCustodianAction(ctx, walBoundProposal("1000MB"),
		custodianBaseline{})
	if err == nil || criterion != "custodian_wal" {
		t.Fatalf("post-check = %q, %v; want a failed custodian_wal check", criterion, err)
	}
	for _, part := range []string{"not effective", "939524096", "1048576000"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q does not name %q", err, part)
		}
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("gave up after %v, want about the 400 ms bound", elapsed)
	}
}

// awaitVisible waits until three reads in a row see the value.
func awaitVisible(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want string) {
	t.Helper()
	seen := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var got string
		if err := pool.QueryRow(ctx,
			"SELECT current_setting('max_slot_wal_keep_size')").Scan(&got); err != nil {
			t.Fatalf("read setting: %v", err)
		}
		if got != want {
			seen = 0
		} else if seen++; seen == 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("max_slot_wal_keep_size never showed %s", want)
}

// An unlimited value (-1) is no backstop, whatever the reload does.
func TestVerifyWALBoundRefusesUnlimited(t *testing.T) {
	pool, ctx := requireDB(t)
	resetSlotKeep(t, pool)
	e := New(pool, config.DefaultConfig(), zeroTime(), func(string, string, ...any) {})
	e.settingWait = 100 * time.Millisecond
	_, err := e.verifyCustodianAction(ctx, walBoundProposal("-1"), custodianBaseline{})
	if err == nil || !strings.Contains(err.Error(), "not effective") {
		t.Fatalf("post-check of an unlimited value = %v, want not effective", err)
	}
}
