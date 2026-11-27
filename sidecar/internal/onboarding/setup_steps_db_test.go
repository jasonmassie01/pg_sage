package onboarding

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// ux-04b: sage.onboarding keeps when the wizard's model, channel and
// autonomy steps were first finished, and the within-the-hour ratios read
// them against installed_at.

func TestRecordSetupStepRecordsOnceAndGetExposesIt(t *testing.T) {
	pool, ctx := freshInstall(t)
	st, err := Init(ctx, pool, "app")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if st.ModelConfiguredAt != nil || st.ChannelConfiguredAt != nil ||
		st.AutonomyChosenAt != nil {
		t.Fatalf("new install = %+v, want no setup step recorded", st)
	}
	at := st.InstalledAt.Add(90 * time.Second).UTC().Truncate(time.Microsecond)
	for _, step := range []string{SetupModel, SetupChannel, SetupAutonomy} {
		changed, err := RecordSetupStep(ctx, pool, "app", step, at)
		if err != nil || !changed {
			t.Fatalf("record %s = %v, %v; want changed", step, changed, err)
		}
		again, err := RecordSetupStep(ctx, pool, "app", step, at.Add(time.Hour))
		if err != nil || again {
			t.Fatalf("second record %s = %v, %v; want unchanged", step, again, err)
		}
	}
	got, found, err := Get(ctx, pool, "app")
	if err != nil || !found {
		t.Fatalf("get: found %v err %v", found, err)
	}
	for name, v := range map[string]*time.Time{"model": got.ModelConfiguredAt,
		"channel": got.ChannelConfiguredAt, "autonomy": got.AutonomyChosenAt} {
		if v == nil || !v.Equal(at) {
			t.Fatalf("%s = %v, want the first record %v", name, v, at)
		}
	}
}

func TestRecordSetupStepRefusesBadInput(t *testing.T) {
	pool, ctx := freshInstall(t)
	if _, err := RecordSetupStep(ctx, pool, "app", SetupModel, time.Now()); !errors.Is(err,
		ErrNotInitialized) {
		t.Fatalf("no row: err = %v, want ErrNotInitialized", err)
	}
	if _, err := Init(ctx, pool, "app"); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, step := range []string{"", "Model", "model; DROP TABLE sage.onboarding",
		"first_look"} {
		if _, err := RecordSetupStep(ctx, pool, "app", step, time.Now()); !errors.Is(err,
			ErrUnknownSetupStep) {
			t.Fatalf("step %q: err = %v, want ErrUnknownSetupStep", step, err)
		}
	}
	if _, err := RecordSetupStep(ctx, nil, "app", SetupModel, time.Now()); !errors.Is(err,
		ErrNoPool) {
		t.Fatalf("nil pool: err = %v, want ErrNoPool", err)
	}
	if _, err := RecordSetupStep(ctx, pool, "", SetupModel, time.Now()); !errors.Is(err,
		ErrNoDatabase) {
		t.Fatalf("no database: err = %v, want ErrNoDatabase", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.onboarding`).Scan(&n); err != nil ||
		n != 1 {
		t.Fatalf("onboarding rows = %d (%v), want the table intact with one row", n, err)
	}
}

// Two admins finishing the same step at once: exactly one write lands
// and the stored time is that write's.
func TestRecordSetupStepConcurrentWritersKeepOne(t *testing.T) {
	pool, ctx := freshInstall(t)
	st, err := Init(ctx, pool, "app")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	const writers = 8
	var wg sync.WaitGroup
	changed := make([]bool, writers)
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			at := st.InstalledAt.Add(time.Duration(i+1) * time.Minute)
			changed[i], errs[i] = RecordSetupStep(ctx, pool, "app", SetupAutonomy, at)
		}(i)
	}
	wg.Wait()
	won := -1
	for i := range writers {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if changed[i] {
			if won >= 0 {
				t.Fatalf("writers %d and %d both recorded the step", won, i)
			}
			won = i
		}
	}
	got, _, err := Get(ctx, pool, "app")
	if err != nil || won < 0 || got.AutonomyChosenAt == nil {
		t.Fatalf("winner %d, state %+v, err %v", won, got, err)
	}
	want := st.InstalledAt.Add(time.Duration(won+1) * time.Minute)
	if !got.AutonomyChosenAt.Equal(want) {
		t.Fatalf("autonomy_chosen_at = %v, want the winner's %v", got.AutonomyChosenAt, want)
	}
}

// Boundary: 3,600 s after installed_at is within the hour; 3,601 s is
// not. The ratios read the stored rows through Get.
func TestSetupWithinHourBoundaryOnStoredRows(t *testing.T) {
	restore := setups
	setups = newSetupGauge()
	t.Cleanup(func() { setups = restore })
	pool, ctx := freshInstall(t)
	st, err := Init(ctx, pool, "app")
	if err != nil || st.InstallKind != InstallNew {
		t.Fatalf("init: %+v %v", st, err)
	}
	if _, err := RecordSetupStep(ctx, pool, "app", SetupModel,
		st.InstalledAt.Add(3600*time.Second)); err != nil {
		t.Fatalf("model: %v", err)
	}
	if _, err := RecordSetupStep(ctx, pool, "app", SetupChannel,
		st.InstalledAt.Add(3601*time.Second)); err != nil {
		t.Fatalf("channel: %v", err)
	}
	r := SetupRatios()
	if r.Installs != 1 || r.Model != 1 || r.Channel != 0 {
		t.Fatalf("ratios = %+v, want 1 install, model 1 (3600 s), channel 0 (3601 s)", r)
	}
}
