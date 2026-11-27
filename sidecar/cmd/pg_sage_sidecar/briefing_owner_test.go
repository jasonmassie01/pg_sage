package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/config"
)

// llm-06: briefing.* is reconfigured live by the "briefing" owner.

func noopBriefingLog(string, string, ...any) {}

func withBriefingOwner(t *testing.T, schedule string) *config.Config {
	t.Helper()
	oldController, oldOwner := configController, briefingWorkers
	t.Cleanup(func() { configController, briefingWorkers = oldController, oldOwner })
	initial := config.DefaultConfig()
	initial.Briefing.Schedule = schedule
	configController = config.NewConfigController(initial, nil)
	briefingWorkers = newBriefingOwner()
	registerBriefingOwner()
	return initial
}

func applyBriefing(t *testing.T, base *config.Config, schedule string) error {
	t.Helper()
	next := config.Clone(base)
	next.Briefing.Schedule = schedule
	res, err := configController.Apply(context.Background(),
		configController.Desired().Generation, next)
	if err == nil && len(res.PendingRestart) != 0 {
		t.Fatalf("pending restart = %v, want a live change", res.PendingRestart)
	}
	return err
}

// State transition: a schedule change through the controller changes the
// next ShouldRun result without a restart.
func TestBriefingScheduleAppliesLive(t *testing.T) {
	initial := withBriefingOwner(t, "0 6 30 2 *") // never fires
	w := briefing.New(nil, initial, nil, noopBriefingLog)
	briefingWorkers.add(w)
	w.MarkRan()
	at := time.Now().Add(3 * time.Minute)
	if w.ShouldRun(at) {
		t.Fatal("a never-firing schedule fired")
	}
	lc, ok := configController.EffectiveLifecycle("briefing.schedule")
	if !ok || lc != config.LifecycleReconfigure {
		t.Fatalf("EffectiveLifecycle(briefing.schedule) = %q (%v), want reconfigure", lc, ok)
	}
	if err := applyBriefing(t, initial, "* * * * *"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !w.ShouldRun(at) {
		t.Fatal("the every-minute schedule did not fire after a live change")
	}
	// A database added after the change follows the active config.
	late := briefing.New(nil, initial, nil, noopBriefingLog)
	briefingWorkers.add(late)
	late.MarkRan()
	if !late.ShouldRun(at) {
		t.Fatal("a worker added after the change kept the startup schedule")
	}
}

// Invalid input: an invalid schedule is rejected at apply time, naming the
// field, and the workers keep the old schedule.
func TestBriefingInvalidScheduleRejectedAtApply(t *testing.T) {
	initial := withBriefingOwner(t, "* * * * *")
	w := briefing.New(nil, initial, nil, noopBriefingLog)
	briefingWorkers.add(w)
	generation := configController.Desired().Generation
	err := applyBriefing(t, initial, "61 * * * *")
	if err == nil || !strings.Contains(err.Error(), "briefing.schedule") {
		t.Fatalf("apply(61 * * * *) = %v, want an error naming briefing.schedule", err)
	}
	if configController.Desired().Generation != generation {
		t.Fatal("a rejected schedule advanced the desired generation")
	}
	w.MarkRan()
	if !w.ShouldRun(time.Now().Add(3 * time.Minute)) {
		t.Fatal("the worker lost its schedule after a rejected change")
	}
}

// Nil/empty: an owner with no workers still validates and applies.
func TestBriefingOwnerWithoutWorkers(t *testing.T) {
	initial := withBriefingOwner(t, "0 6 * * *")
	if err := applyBriefing(t, initial, "0 7 * * *"); err != nil {
		t.Fatalf("apply with no workers: %v", err)
	}
	if got := briefingWorkers.workers(); len(got) != 0 {
		t.Fatalf("workers = %v, want none", got)
	}
}

// A runtime's worker leaves the owner when the runtime stops.
func TestBriefingOwnerTrackDropsStoppedWorker(t *testing.T) {
	withBriefingOwner(t, "0 6 * * *")
	ctx, cancel := context.WithCancel(context.Background())
	w := briefing.New(nil, config.DefaultConfig(), nil, noopBriefingLog).
		WithDatabaseName("db1")
	done := briefingWorkers.track(ctx, w)
	if got := briefingWorkers.workers(); len(got) != 1 || got[0] != w {
		t.Fatalf("workers = %v, want the tracked worker", got)
	}
	cancel()
	<-done
	if got := briefingWorkers.workers(); len(got) != 0 {
		t.Fatalf("workers after stop = %v, want none", got)
	}
}

// Fleet roll-up is the default in fleet and meta-db mode; per_database and
// standalone keep one message per database.
func TestBriefingRollupSelection(t *testing.T) {
	w := briefing.New(nil, config.DefaultConfig(), nil, noopBriefingLog)
	tests := []struct {
		mode, fleetMode string
		rollup          bool
	}{
		{"fleet", "", true},
		{"fleet", config.BriefingFleetRollup, true},
		{"fleet", config.BriefingFleetPerDatabase, false},
		{"standalone", config.BriefingFleetRollup, false},
		{"standalone", "", false},
		// A meta-db fleet is a fleet: mode meta, inferred from meta_db.
		{config.ModeMeta, "", true},
		{config.ModeMeta, config.BriefingFleetPerDatabase, false},
	}
	for _, tt := range tests {
		c := config.DefaultConfig()
		c.Mode, c.Briefing.FleetMode = tt.mode, tt.fleetMode
		if tt.mode == config.ModeMeta {
			c.MetaDB = "postgres://meta.invalid/sage"
		}
		if got := briefingRollup(c); got != tt.rollup {
			t.Errorf("%s/%q: briefingRollup = %v, want %v", tt.mode, tt.fleetMode, got, tt.rollup)
		}
		got := cycleBriefing(c, w)
		if tt.rollup && got != nil {
			t.Errorf("%s/%q: the per-database cycle still briefs in roll-up mode",
				tt.mode, tt.fleetMode)
		}
		if !tt.rollup && got != w {
			t.Errorf("%s/%q: the per-database cycle lost its briefing", tt.mode, tt.fleetMode)
		}
	}
	if briefingRollup(nil) {
		t.Fatal("briefingRollup(nil) = true")
	}
}

// No concurrent access test here: the worker's settings swap is covered by
// briefing.TestReconfigureIsNeverTorn; the owner's worker set is guarded
// by one mutex and exercised by the tests above.
