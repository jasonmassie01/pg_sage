package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// Fast elevation wiring: sre.autonomy.promotion maps onto the ledger's
// thresholds (the spec bar by default), and the sidecar warns loudly at
// startup exactly when an elevation setting is faster than the spec, with
// one line per lowered value, so fast elevation is never silent.

func TestPromotionThresholdsDefaultToTheSpec(t *testing.T) {
	got := promotionThresholds(config.DefaultConfig().SRE.Autonomy.Promotion)
	if got != earned.DefaultThresholds() {
		t.Fatalf("default promotion thresholds = %+v, want the spec %+v", got,
			earned.DefaultThresholds())
	}
}

func TestPromotionThresholdsMapEveryKnob(t *testing.T) {
	p := config.SREPromotionConfig{ShadowWindowHours: 4, ShadowMinReviewed: 3,
		ShadowMinAcceptedPct: 90, BenchMinTop1Pct: 75, BenchMinPrecisionPct: 85,
		MinSafePassPct: 92, MinLiveRecoveries: 3}
	got := promotionThresholds(p)
	spec := earned.DefaultThresholds()
	if got.ShadowDuration != 4*time.Hour || got.ShadowMinReviewed != 3 ||
		got.ShadowMinAccepted != 0.90 || got.MinTop1 != 0.75 || got.MinPrecision != 0.85 ||
		got.MinSafePass != 0.92 || got.GameDayMinSafePass != 0.92 ||
		got.MinL2Recoveries != 3 {
		t.Fatalf("mapped thresholds = %+v", got)
	}
	if got.BenchMaxAge != spec.BenchMaxAge || got.MinTop1N != spec.MinTop1N ||
		got.MinSafePassN != spec.MinSafePassN {
		t.Fatalf("fixed thresholds changed: %+v", got)
	}
}

func TestAutonomyServiceConfigCarriesThePromotionBar(t *testing.T) {
	s := config.DefaultConfig().SRE.Autonomy
	s.Promotion.ShadowWindowHours, s.Promotion.MinLiveRecoveries = 4, 3
	c := autonomyServiceConfig(s)
	if c.Thresholds.ShadowDuration != 4*time.Hour || c.Thresholds.MinL2Recoveries != 3 {
		t.Fatalf("service thresholds = %+v", c.Thresholds)
	}
}

type warnCapture struct{ lines []string }

func (w *warnCapture) warn(component, format string, args ...any) {
	w.lines = append(w.lines, component+": "+fmt.Sprintf(format, args...))
}

func TestFastElevationWarningSilentAtSpecDefaults(t *testing.T) {
	var w warnCapture
	if got := warnFastElevation(config.DefaultConfig(), w.warn); len(got) != 0 ||
		len(w.lines) != 0 {
		t.Fatalf("spec defaults warned: %v / %v", got, w.lines)
	}
	if got := warnFastElevation(nil, w.warn); len(got) != 0 || len(w.lines) != 0 {
		t.Fatalf("nil config warned: %v / %v", got, w.lines)
	}
}

func TestFastElevationWarningListsEachLoweredValue(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 1, 4
	cfg.SRE.Autonomy.Promotion.MinLiveRecoveries = 3
	var w warnCapture
	lowered := warnFastElevation(cfg, w.warn)
	if len(lowered) != 3 || len(w.lines) != 4 {
		t.Fatalf("lowered %v, warnings %q: want 3 values and a headline", lowered, w.lines)
	}
	if !strings.Contains(w.lines[0], "FAST ELEVATION") ||
		!strings.Contains(w.lines[0], "3 ") {
		t.Fatalf("headline = %q", w.lines[0])
	}
	for i, want := range []string{"trust.ramp_safe_hours = 1 hours (spec default 192)",
		"trust.ramp_moderate_hours = 4 hours (spec default 744)",
		"sre.autonomy.promotion.min_live_recoveries = 3 recoveries (spec default 50)"} {
		if !strings.Contains(w.lines[i+1], want) {
			t.Errorf("warning %d = %q, want %q", i+1, w.lines[i+1], want)
		}
	}
	for _, line := range w.lines {
		if !strings.HasPrefix(line, "startup: ") {
			t.Errorf("warning %q is not a startup warning", line)
		}
	}
}

func TestFastElevationWarningSilentWhenOnlyRaised(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.RampSafeHours, cfg.Trust.RampModerateHours = 500, 1000
	cfg.SRE.Autonomy.Promotion.MinLiveRecoveries = 100
	var w warnCapture
	if got := warnFastElevation(cfg, w.warn); len(got) != 0 || len(w.lines) != 0 {
		t.Fatalf("raised settings warned: %v / %q", got, w.lines)
	}
}

func TestAutonomyAPIDepsShowFastElevation(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = config.DefaultConfig()
	if deps := autonomyAPIDeps(nil, nil); deps.FastElevation == nil ||
		len(deps.FastElevation) != 0 {
		t.Fatalf("spec config fast elevation = %#v, want an empty list", deps.FastElevation)
	}
	cfg.Trust.RampSafeHours = 2
	deps := autonomyAPIDeps(nil, nil)
	if len(deps.FastElevation) != 1 || deps.FastElevation[0].Key != "trust.ramp_safe_hours" ||
		deps.FastElevation[0].Value != 2 || deps.FastElevation[0].Default != 192 {
		t.Fatalf("fast elevation = %+v", deps.FastElevation)
	}
}
