package main

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre"
)

// sreTriggerSource is the investigator's trigger source: RCA incidents
// and plan regression findings, plus the M6 reactive detector for the
// families without an RCA signal of their own (checkpoint storms,
// temp-file growth, LWLock contention). The detector samples through the
// database's probe runner, only while automatic start polls triggers.
func sreTriggerSource(d sreInvestigatorDeps) (sre.TriggerSource, error) {
	detector, err := sre.NewReactiveDetector(d.runner, d.name, sre.DefaultDetectorConfig(),
		d.logFn)
	if err != nil {
		return nil, fmt.Errorf("sre reactive detector: %w", err)
	}
	return sre.CombineTriggers(d.logFn, sre.NewPGTriggerSource(d.monitored, d.name),
		detector), nil
}
