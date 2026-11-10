package gameday

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clusterlock"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// helperDSNEnv tells TestBenchTargetHolderProcess, run as a subprocess,
// which server to hold the bench target lock on.
const helperDSNEnv = "GAMEDAY_TARGET_HOLDER_DSN"

// TestBenchTargetHolderProcess is not a test of its own: run as a
// subprocess (helperDSNEnv set) it is another pg_sage process holding the
// bench target until it is killed. Run directly it returns at once.
func TestBenchTargetHolderProcess(t *testing.T) {
	dsn := os.Getenv(helperDSNEnv)
	if dsn == "" {
		return
	}
	release, ok, err := clusterlock.TryLock(context.Background(), dsn, BenchTargetLock)
	if err != nil {
		t.Fatalf("helper: lock the bench target: %v", err)
	}
	if !ok {
		os.Stdout.WriteString("refused\n")
		os.Exit(3)
	}
	defer release()
	os.Stdout.WriteString("held\n")
	time.Sleep(2 * time.Minute)
}

// startHolder runs TestBenchTargetHolderProcess against dsn and waits
// until it holds the lock.
func startHolder(t *testing.T, dsn string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBenchTargetHolderProcess$",
		"-test.count=1")
	cmd.Env = append(os.Environ(), helperDSNEnv+"="+dsn,
		"SAGE_TEST_DATABASE_URL=", "SAGE_DATABASE_URL=")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			if txt := strings.TrimSpace(s.Text()); txt == "held" || txt == "refused" {
				line <- txt
				return
			}
		}
		line <- "exited"
	}()
	select {
	case got := <-line:
		if got != "held" {
			t.Fatalf("the holder process %s", got)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the holder process never took the lock")
	}
	return cmd
}

// Integration: another process holds the local target. This process's
// game day is refused with nothing stored and no fault program run (so
// nothing can demote a family); when the holder dies its lease ends with
// its connection, and the target can be leased again.
func TestLeaseAcrossProcessesOnRealPostgres(t *testing.T) {
	f := newGDFixture(t)
	dsn := testdb.SkipUnlessLive(t)
	p, err := NewLocalProvider(dsn, nil, refusing(prodCluster()))
	if err != nil {
		t.Fatal(err)
	}
	holder := startHolder(t, dsn)
	r := f.localRunner(p, f.faults)
	_, err = r.Run(f.ctx)
	wantBusy(t, "game day while another process holds the target", err, "another session")
	wantNoGameDays(t, r)
	if len(f.faults.dsns) != 0 {
		t.Fatalf("fault programs ran on a held target: %d", len(f.faults.dsns))
	}
	runs, err := f.ledger.Store().GameDayRuns(f.ctx, f.now.Add(-time.Hour))
	if err != nil || len(runs) != 0 {
		t.Fatalf("a refused game day reached the ledger: %+v (%v)", runs, err)
	}
	_ = holder.Process.Kill()
	_ = holder.Wait()
	deadline := time.Now().Add(15 * time.Second)
	for {
		release, err := processLeases.Acquire(f.ctx, p, Holder{Kind: "test", ID: "probe",
			Database: f.database})
		if err == nil {
			release()
			return
		}
		if !errors.Is(err, ErrRunning) || time.Now().After(deadline) {
			t.Fatalf("the dead holder's lease never ended: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
