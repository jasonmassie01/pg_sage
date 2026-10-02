package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre.detectors.* reaches the reactive detector every runtime mode builds
// (CHECK-31): each knob is mapped, a section that was never loaded keeps
// the detector's conservative defaults, and a window too short to see two
// trigger polls is reported at startup instead of silently never firing.

func TestSREDetectorConfig_MapsEveryKnob(t *testing.T) {
	s := config.DefaultConfig().SRE
	s.Detectors = config.SREDetectorsConfig{WindowSeconds: 600, CheckpointRequested: 7,
		TempFileMB: 4096, LWLockWaiters: 20, LWLockPolls: 5, CooldownMinutes: 90}
	got := sreDetectorConfig(s)
	want := sre.DetectorConfig{Window: 10 * time.Minute, CheckpointRequested: 7,
		TempBytes: 4096 << 20, LWLockWaiters: 20, LWLockPolls: 5, Cooldown: 90 * time.Minute}
	if got != want {
		t.Fatalf("detector config = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("mapped config invalid: %v", err)
	}
}

func TestSREDetectorConfig_DefaultsMatchTheDetector(t *testing.T) {
	if got := sreDetectorConfig(config.DefaultConfig().SRE); got != sre.DefaultDetectorConfig() {
		t.Fatalf("loaded defaults map to %+v, want %+v", got, sre.DefaultDetectorConfig())
	}
	if got := sreDetectorConfig(config.SREConfig{}); got != sre.DefaultDetectorConfig() {
		t.Fatalf("an unloaded section maps to %+v, want the defaults", got)
	}
}

// noProbes answers nothing; the trigger source is only built here.
type noProbes struct{}

func (noProbes) Run(context.Context, probes.ID, probes.Args) probes.Result {
	return probes.Result{Status: probes.StatusEmpty, Reason: "no_rows"}
}

type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) logFn(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *lineLog) matching(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range l.lines {
		if strings.Contains(s, substr) {
			out = append(out, s)
		}
	}
	return out
}

func TestSRETriggerSource_WarnsWhenTheWindowCannotSeeGrowth(t *testing.T) {
	cases := []struct {
		trigger, window int
		warn            bool
	}{{15, 300, false}, {150, 300, false}, {151, 300, true}, {600, 300, true}}
	for _, c := range cases {
		s := config.DefaultConfig().SRE
		s.TriggerIntervalSeconds, s.Detectors.WindowSeconds = c.trigger, c.window
		logs := &lineLog{}
		if _, err := sreTriggerSource(sreInvestigatorDeps{runner: noProbes{}, name: "w",
			settings: s, logFn: logs.logFn}); err != nil {
			t.Fatalf("trigger source: %v", err)
		}
		got := logs.matching("sre.detectors.window_seconds")
		if c.warn && (len(got) != 1 || !strings.HasPrefix(got[0], "WARN")) {
			t.Errorf("trigger %d s, window %d s: logs %v, want one WARN", c.trigger,
				c.window, logs.lines)
		}
		if !c.warn && len(got) != 0 {
			t.Errorf("trigger %d s, window %d s: unexpected warning %v", c.trigger, c.window,
				got)
		}
	}
}

// Invalid thresholds that bypass config validation never build a
// detector that fires on noise.
func TestSRETriggerSource_RefusesAnInvalidDetector(t *testing.T) {
	s := config.DefaultConfig().SRE
	s.Detectors.LWLockWaiters = -1
	_, err := sreTriggerSource(sreInvestigatorDeps{runner: noProbes{}, name: "w",
		settings: s, logFn: func(string, string, ...any) {}})
	if err == nil || !strings.Contains(err.Error(), "detector") {
		t.Fatalf("err = %v, want the detector refused", err)
	}
}
