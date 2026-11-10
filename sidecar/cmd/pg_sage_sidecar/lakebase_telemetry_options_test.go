package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestLakebaseTelemetryOptionsCarryScheduleKeys(t *testing.T) {
	got := lakebaseTelemetryOptions(config.LakebaseObservabilityConfig{WarehouseID: "w",
		SettleLagSeconds: 90, ShortLookbackSeconds: 3600, QueriesPerPoll: 20})
	if got.SettleLag != 90*time.Second || got.ShortLookback != time.Hour ||
		got.QueriesPerPoll != 20 {
		t.Fatalf("options settle=%v short=%v budget=%d", got.SettleLag, got.ShortLookback,
			got.QueriesPerPoll)
	}
	// Zero keys stay zero so lakebaseobs applies its own defaults.
	zero := lakebaseTelemetryOptions(config.LakebaseObservabilityConfig{WarehouseID: "w"})
	if zero.SettleLag != 0 || zero.ShortLookback != 0 || zero.QueriesPerPoll != 0 {
		t.Fatalf("zero config produced %+v", zero)
	}
}
