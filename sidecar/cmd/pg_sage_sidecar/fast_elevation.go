package main

import (
	"strconv"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
)

// Fast elevation: an operator may let a dogfood database earn trust in
// hours. It is never silent: every lowered value is a startup WARN and is
// listed by the autonomy API and UI.

// promotionThresholds maps sre.autonomy.promotion onto the ledger's
// promotion bar. The bench report age and the minimum bench run counts
// stay the spec's.
func promotionThresholds(p config.SREPromotionConfig) earned.Thresholds {
	th := earned.DefaultThresholds()
	th.ShadowDuration = p.ShadowWindow()
	th.ShadowMinReviewed = p.ShadowMinReviewed
	th.ShadowMinAccepted = p.ShadowMinAcceptedPct / 100
	th.MinTop1 = p.BenchMinTop1Pct / 100
	th.MinPrecision = p.BenchMinPrecisionPct / 100
	th.MinSafePass = p.MinSafePassPct / 100
	th.GameDayMinSafePass = p.MinSafePassPct / 100
	th.MinL2Recoveries = p.MinLiveRecoveries
	return th.Normalized()
}

// warnFastElevation logs a headline and one line per elevation setting
// below the spec, and returns them; nothing when none is lowered.
func warnFastElevation(c *config.Config,
	warn func(component, format string, args ...any)) []config.LoweredSetting {
	lowered := c.LoweredElevation()
	if len(lowered) == 0 {
		return lowered
	}
	warn("startup", "FAST ELEVATION: %d trust-elevation settings are below the spec "+
		"defaults; pg_sage can earn autonomy in hours. Irreversible actions, L4, the "+
		"emergency stop and admin approval of promotions are unchanged", len(lowered))
	for _, l := range lowered {
		warn("startup", "FAST ELEVATION: %s = %s %s (spec default %s)", l.Key,
			formatSetting(l.Value), l.Unit, formatSetting(l.Default))
	}
	return lowered
}

func formatSetting(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
