package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

func TestDatabaseExecConfigAppliesOnlyItsOwnAttestation(t *testing.T) {
	base := config.DefaultConfig()
	attested := config.DatabaseConfig{Name: "orders", Verify: config.DatabaseVerifyConfig{
		IOCapacity: &config.IOCapacityConfig{ReadWriteMBps: 500, WALMBps: 125},
	}}
	orders := databaseExecConfig(base, attested)
	billing := databaseExecConfig(base, config.DatabaseConfig{Name: "billing"})
	if orders.Verify.IOCapacity == nil || orders.Verify.IOCapacity.ReadWriteMBps != 500 {
		t.Fatalf("orders capacity = %+v", orders.Verify.IOCapacity)
	}
	if billing.Verify.IOCapacity != nil || base.Verify.IOCapacity != nil {
		t.Fatalf("attestation leaked: billing=%+v base=%+v",
			billing.Verify.IOCapacity, base.Verify.IOCapacity)
	}
	orders.Verify.IOCapacity.ReadWriteMBps = 1
	if attested.Verify.IOCapacity.ReadWriteMBps != 500 {
		t.Fatal("exec config aliases the database attestation")
	}
}

func TestStartIOAdmissionWithoutPoolInstallsNothing(t *testing.T) {
	var workers sync.WaitGroup
	exec := executor.New(nil, config.DefaultConfig(), time.Time{},
		func(string, string, ...any) {})
	if monitor := startIOAdmission(context.Background(), &workers, nil,
		config.DefaultConfig(), "db", exec); monitor != nil {
		t.Fatal("sampler started without a database pool")
	}
	status := exec.IndexAdmissionStatus(context.Background())
	if !slices.Contains(status.MissingEvidence, "pg_io_rate") {
		t.Fatalf("missing evidence = %v", status.MissingEvidence)
	}
}

func TestStartIOAdmissionSamplesAndStops(t *testing.T) {
	pool := preflightRuntimePool(t)
	cfg := config.DefaultConfig()
	exec := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	name := "io_admission_wiring_" + time.Now().Format("150405.000000")
	exec.WithDatabaseName(name)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.io_rate_sample WHERE database_name=$1", name)
	})
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	monitor := startIOAdmission(ctx, &workers, pool, cfg, name, exec)
	if monitor == nil {
		cancel()
		t.Fatal("sampler not started")
	}
	status := exec.IndexAdmissionStatus(context.Background())
	if slices.Contains(status.MissingEvidence, "pg_io_rate") {
		t.Fatalf("installed sampler still reported missing: %v", status.MissingEvidence)
	}
	// The worker samples immediately: the first reading primes the interval.
	deadline := time.Now().Add(10 * time.Second)
	var rateError string
	for time.Now().Before(deadline) {
		evidence, err := monitor.IOEvidence(context.Background())
		if err != nil {
			cancel()
			t.Fatalf("IOEvidence: %v", err)
		}
		if rateError = evidence.RateError; strings.Contains(rateError, "priming") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(rateError, "priming") {
		cancel()
		t.Fatalf("sampler never ran: rate error %q", rateError)
	}
	cancel()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sampler worker did not stop on cancellation")
	}
}
