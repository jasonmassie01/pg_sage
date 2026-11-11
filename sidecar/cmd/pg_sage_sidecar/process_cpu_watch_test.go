package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The watch's reporter logs a refusal once while it repeats, logs a
// different one, and logs the same refusal again after a clean tick.
func TestCPUWatchReporter_LogsChangesAndForgetsOnRecovery(t *testing.T) {
	var logged []string
	report := cpuWatchReporter(func(err error) { logged = append(logged, err.Error()) })
	refused, other := errors.New("db1 refused"), errors.New("db2 refused")
	for _, err := range []error{nil, refused, refused, other, refused, nil, refused} {
		report(err)
	}
	want := []string{"db1 refused", "db2 refused", "db1 refused", "db1 refused"}
	if strings.Join(logged, "|") != strings.Join(want, "|") {
		t.Fatalf("logged %q, want %q", logged, want)
	}
}

// Boundary: a database that holds a lock on sage.findings cannot stall
// the watch that serves every database. Each write to it gives up at
// cpuSinkWriteTimeout and the finding lands on the next database.
func TestFleetCPUSink_HungDatabaseIsBounded(t *testing.T) {
	r := newFleetCPURig(t)
	saved := cpuSinkWriteTimeout
	cpuSinkWriteTimeout = 500 * time.Millisecond
	t.Cleanup(func() { cpuSinkWriteTimeout = saved })
	tx, err := r.pools["db1"].Begin(r.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(r.ctx, "LOCK TABLE sage.findings IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock db1's findings: %v", err)
	}
	r.tick(t, 0, 0) // first observation: unknown, writes nothing
	r.cpu.burn(time.Minute, time.Hour)
	start := time.Now()
	err = r.watch.Tick(r.ctx)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("tick took %v against a locked database, want it bounded", took)
	}
	if err == nil || !strings.Contains(err.Error(), "on db1") {
		t.Fatalf("tick error = %v, want db1's refused write named", err)
	}
	if got := r.watch.Reading(); got.Database != "db2" {
		t.Fatalf("finding on %q, want db2 while db1 is locked", got.Database)
	}
	_ = tx.Rollback(context.Background())
	if counts, _ := r.cpuFindings(t); counts["db1"] != 0 || counts["db2"] != 1 {
		t.Fatalf("findings = %v, want one on db2 only", counts)
	}
}
