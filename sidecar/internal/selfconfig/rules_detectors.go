package selfconfig

import (
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/config"
)

const mib = 1 << 20

// tempMiBPerWindow is the database's temp-file traffic per detector window.
func tempMiBPerWindow(ev Evidence, cfg *config.Config) (float64, bool) {
	if !usable(ev.TempBytesPerSecond) {
		return 0, false
	}
	window := float64(cfg.SRE.Detectors.WindowSeconds)
	if window <= 0 {
		window = float64(config.DefaultConfig().SRE.Detectors.WindowSeconds)
	}
	return ev.TempBytesPerSecond.Value * window / mib, true
}

func tempFileRule() Rule {
	return Rule{
		Key: "sre.detectors.temp_file_mb", Name: "temp_threshold_by_workload", Version: 1,
		Unit: "MiB", Summary: "Temp-file explosion threshold at 4x this database's normal " +
			"temp traffic per detector window, so routine sorts and hashes are not incidents.",
		EvidenceDoc: "pg_stat_database.temp_bytes rate (since the previous sample, or " +
			"since the statistics reset) over sre.detectors.window_seconds.",
		OutcomeDoc: "share of soak windows over each threshold: raise only when >=10% " +
			"cross the active one and <=5% cross the candidate",
		Widens: WidensWhenLower, Cap: CapAtDefault, Step: 256, HardMin: 1024, HardMax: 65536,
		Cites: []string{"temp_bytes_per_second", "temp_bytes", "stats_age_seconds"},
		Derive: func(ev Evidence, cfg *config.Config) Derivation {
			perWindow, ok := tempMiBPerWindow(ev, cfg)
			if !ok {
				return Derivation{Reason: "the temp-file rate is not measured"}
			}
			return Derivation{OK: true, Value: perWindow * 4, Reason: fmt.Sprintf(
				"this database writes %.0f MiB of temp files per %ds window; the "+
					"threshold is 4x that", perWindow, cfg.SRE.Detectors.WindowSeconds)}
		},
		Observe: tempMiBPerWindow,
		Compare: func(active, candidate float64, samples []float64, _ *config.Config) Verdict {
			overActive := shareAbove(samples, active)
			overCandidate := shareAbove(samples, candidate)
			if candidate > active {
				return verdict(overActive >= noisyShare && overCandidate <= quietShare,
					"%.0f%% of soak windows crossed %g MiB and %.0f%% crossed %g MiB",
					overActive*100, active, overCandidate*100, candidate)
			}
			return verdict(overCandidate <= quietShare,
				"%.0f%% of soak windows crossed %g MiB", overCandidate*100, candidate)
		},
		Get: func(c *config.Config) float64 { return float64(c.SRE.Detectors.TempFileMB) },
		Set: func(c *config.Config, v float64) { c.SRE.Detectors.TempFileMB = int(v) },
	}
}

func lwlockWaitersRule() Rule {
	return Rule{
		Key: "sre.detectors.lwlock_waiters", Name: "lwlock_threshold_by_connections",
		Version: 1, Unit: "backends", Summary: "LWLock contention threshold at 2% of the " +
			"server's connection limit: 8 waiters is contention on a 100-connection " +
			"server and noise on a 2000-connection one.",
		EvidenceDoc: "the server's max_connections.",
		OutcomeDoc:  "none measurable (real contention is rare); the soak is the test",
		Widens:      WidensWhenLower, Cap: CapAtDefault, Step: 1, HardMin: 8, HardMax: 64,
		Cites: []string{"max_connections"},
		Derive: func(ev Evidence, _ *config.Config) Derivation {
			if !usable(ev.MaxConnections) || ev.MaxConnections.Value < 1 {
				return Derivation{Reason: "max_connections is not known"}
			}
			v := math.Ceil(ev.MaxConnections.Value/lwlockShare - 1e-9)
			return Derivation{OK: true, Value: v, Reason: fmt.Sprintf(
				"max_connections is %.0f; 2%% of it is %.0f waiting backends",
				ev.MaxConnections.Value, v)}
		},
		// Nothing to sample: an episode is rare and says nothing about the
		// threshold's noise.
		Observe: func(Evidence, *config.Config) (float64, bool) { return 0, false },
		Get:     func(c *config.Config) float64 { return float64(c.SRE.Detectors.LWLockWaiters) },
		Set:     func(c *config.Config, v float64) { c.SRE.Detectors.LWLockWaiters = int(v) },
	}
}
