package fleet

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// A runtime starts its workers before RegisterInstance publishes it, and
// those workers (the deferred index builder) poll InstanceStopped. Restoring
// a persisted stop must therefore write the latch under the manager lock:
// go test -race reported the unlocked write in the meta-db parity test. The
// poller must also never see the latch go from stopped back to running.
func TestRegisterRestoreWhileAWorkerPollsTheLatch(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		state  executor.EmergencyStopState
		err    error
		wantBy string
	}{
		{"persisted stop", executor.EmergencyStopState{Stopped: true,
			UpdatedBy: "op@example.com", UpdatedAt: at}, nil, "op@example.com"},
		{"unreadable flag fails closed", executor.EmergencyStopState{},
			errors.New("statement timeout"), executor.EmergencyStopActorSystem},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(&config.Config{Mode: "fleet"})
			inst := &DatabaseInstance{
				Name:     "orders",
				Config:   config.DatabaseConfig{Name: "orders"},
				Executor: newGateExecutor(),
				Status:   &InstanceStatus{Connected: true, Platform: "postgres"},
			}
			polling := make(chan struct{})
			mgr.readStop = func(context.Context, *DatabaseInstance) (
				executor.EmergencyStopState, error) {
				<-polling // the worker is already reading the latch
				return tc.state, tc.err
			}
			mgr.readScope = func(context.Context, *DatabaseInstance) (bool, error) {
				return false, nil
			}
			flips := pollLatchDuring(t, mgr, inst, polling, func() {
				mgr.RegisterInstance(inst)
			})
			if flips != 0 {
				t.Fatalf("the worker saw the restored stop released %d times", flips)
			}
			if !mgr.InstanceStopped(inst) || inst.Executor.ExecutorEnabled() {
				t.Fatal("the restored stop is not latched or the executor still runs")
			}
			if inst.StoppedBy != tc.wantBy {
				t.Fatalf("StoppedBy = %q, want %q", inst.StoppedBy, tc.wantBy)
			}
		})
	}
}

// pollLatchDuring reads InstanceStopped in a loop, as a runtime worker does,
// while register runs. It closes polling once the first read is done and
// returns how often the latch went from stopped back to running.
func pollLatchDuring(t *testing.T, mgr *DatabaseManager, inst *DatabaseInstance,
	polling chan struct{}, register func()) int {
	t.Helper()
	var done atomic.Bool
	var flips int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		seen := false
		for first := true; ; first = false {
			stopped := mgr.InstanceStopped(inst)
			if seen && !stopped {
				flips++
			}
			seen = seen || stopped
			if first {
				close(polling)
			}
			if done.Load() {
				return
			}
		}
	}()
	register()
	done.Store(true)
	wg.Wait()
	return flips
}
