package onboarding

import (
	"sync"
	"testing"
	"time"
)

// ux-04b: WithinHour and the in-process setup gauge.

func TestWithinHourBoundaries(t *testing.T) {
	installed := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := installed.Add(d); return &v }
	for name, tc := range map[string]struct {
		at   *time.Time
		want bool
	}{
		"never":          {nil, false},
		"at install":     {at(0), true},
		"3599 s":         {at(3599 * time.Second), true},
		"exactly 3600 s": {at(3600 * time.Second), true},
		"3600 s + 1 ns":  {at(3600*time.Second + time.Nanosecond), false},
		"3601 s":         {at(3601 * time.Second), false},
		// A clock that moved back between the install and the step still
		// finished within the hour.
		"before install": {at(-time.Minute), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := WithinHour(installed, tc.at); got != tc.want {
				t.Fatalf("WithinHour(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestSetupRatiosCountNewInstallsOnly(t *testing.T) {
	restore := setups
	setups = newSetupGauge()
	t.Cleanup(func() { setups = restore })
	if r := SetupRatios(); r.Installs != 0 || r.Model != 0 || r.Channel != 0 {
		t.Fatalf("empty gauge = %+v, want zero installs", r)
	}
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	soon := base.Add(10 * time.Minute)
	late := base.Add(2 * time.Hour)
	ObserveSetup(State{Database: "a", InstallKind: InstallNew, InstalledAt: base,
		ModelConfiguredAt: &soon, ChannelConfiguredAt: &late})
	ObserveSetup(State{Database: "b", InstallKind: InstallNew, InstalledAt: base,
		ChannelConfiguredAt: &soon})
	ObserveSetup(State{Database: "old", InstallKind: InstallExisting, InstalledAt: base,
		ModelConfiguredAt: &soon, ChannelConfiguredAt: &soon})
	ObserveSetup(State{InstallKind: InstallNew, InstalledAt: base}) // no name: ignored
	r := SetupRatios()
	if r.Installs != 2 || r.Model != 0.5 || r.Channel != 0.5 {
		t.Fatalf("ratios = %+v, want 2 new installs, model 0.5, channel 0.5", r)
	}
	// A later read of the same install replaces its marks.
	ObserveSetup(State{Database: "b", InstallKind: InstallNew, InstalledAt: base,
		ModelConfiguredAt: &soon, ChannelConfiguredAt: &soon})
	if r := SetupRatios(); r.Installs != 2 || r.Model != 1 || r.Channel != 0.5 {
		t.Fatalf("after b's update = %+v, want model 1 and channel 0.5 (a's was late)", r)
	}
	ForgetSetup("a")
	ForgetSetup("missing")
	if r := SetupRatios(); r.Installs != 1 || r.Model != 1 || r.Channel != 1 {
		t.Fatalf("after forgetting a = %+v, want b alone", r)
	}
}

func TestSetupGaugeConcurrentObservers(t *testing.T) {
	restore := setups
	setups = newSetupGauge()
	t.Cleanup(func() { setups = restore })
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	soon := base.Add(time.Minute)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(2)
		name := string(rune('a' + i))
		go func() {
			defer wg.Done()
			ObserveSetup(State{Database: name, InstallKind: InstallNew, InstalledAt: base,
				ModelConfiguredAt: &soon})
		}()
		go func() {
			defer wg.Done()
			_ = SetupRatios()
		}()
	}
	wg.Wait()
	if r := SetupRatios(); r.Installs != 16 || r.Model != 1 || r.Channel != 0 {
		t.Fatalf("ratios = %+v, want 16 installs with a model and no channel", r)
	}
}
