package main

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre"
)

// sreTriggerSource is the investigator's trigger source: RCA incidents
// and plan regression findings, plus the M6 reactive detector for the
// families without an RCA signal of their own (checkpoint storms,
// temp-file growth, LWLock contention). The detector samples through the
// database's probe runner, only while automatic start polls triggers,
// with the sre.detectors.* thresholds. With the database's RCA engine as
// episode sink every episode is also an incident.
func sreTriggerSource(d sreInvestigatorDeps) (sre.TriggerSource, error) {
	dc := sreDetectorConfig(d.settings)
	detector, err := sre.NewReactiveDetector(d.runner, d.name, dc, d.logFn)
	if err != nil {
		return nil, fmt.Errorf("sre reactive detector: %w", err)
	}
	if d.episodes != nil {
		detector.WithIncidents(d.episodes)
	}
	warnDetectorWindow(d, dc)
	return sre.CombineTriggers(d.logFn, sre.NewPGTriggerSource(d.monitored, d.name),
		detector), nil
}

// sreDetectorConfig maps sre.detectors.*. A section that was never
// loaded (a zero struct; Load always fills the defaults and refuses
// zeros) keeps the detector's defaults.
func sreDetectorConfig(s config.SREConfig) sre.DetectorConfig {
	d := s.Detectors
	if d == (config.SREDetectorsConfig{}) {
		return sre.DefaultDetectorConfig()
	}
	return sre.DetectorConfig{Window: d.Window(),
		CheckpointRequested: float64(d.CheckpointRequested), TempBytes: float64(d.TempBytes()),
		LWLockWaiters: int64(d.LWLockWaiters), LWLockPolls: d.LWLockPolls,
		Cooldown: d.Cooldown()}
}

// warnDetectorWindow reports a window that cannot hold two trigger polls:
// growth is measured between polls inside the window, so such a detector
// never sees a checkpoint storm or a temp-file explosion. The pair is not
// refused, so an existing slow trigger interval keeps loading.
func warnDetectorWindow(d sreInvestigatorDeps, dc sre.DetectorConfig) {
	poll := d.settings.TriggerInterval()
	if d.logFn == nil || poll <= 0 || dc.Window >= 2*poll {
		return
	}
	d.logFn("WARN", "sre: db %q: sre.detectors.window_seconds (%s) is shorter than two "+
		"trigger polls (sre.trigger_interval_seconds %s): checkpoint storms and temp-file "+
		"explosions cannot be detected; raise the window or poll faster", d.name, dc.Window,
		poll)
}
