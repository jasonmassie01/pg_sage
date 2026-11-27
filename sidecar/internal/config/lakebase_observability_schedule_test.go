package config

import (
	"strings"
	"testing"
)

func TestLakebaseHistoryScheduleKeysAreBounded(t *testing.T) {
	valid := LakebaseObservabilityConfig{Enabled: true, WarehouseID: "warehouse-1",
		Bindings: []LakebaseObservabilityBinding{{Host: "ep-test.database.example.com",
			Database: "sage_test", Endpoint: "projects/search/branches/test/endpoints/primary"}}}
	for _, tc := range []struct {
		name          string
		settle, short int
		wantErr       string
	}{
		{"defaults", 0, 0, ""},
		{"settle one second", 1, 0, ""},
		{"settle one hour", 3600, 0, ""},
		{"settle negative", -1, 0, "settle_lag_seconds"},
		{"settle above one hour", 3601, 0, "settle_lag_seconds"},
		{"short seven days", 0, 604800, ""},
		{"short above seven days", 0, 604801, "short_lookback_seconds"},
		{"short negative", 0, -1, "short_lookback_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			c.SettleLagSeconds, c.ShortLookbackSeconds = tc.settle, tc.short
			err := c.validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("valid schedule refused: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error %v, want refusal naming %s", err, tc.wantErr)
			}
		})
	}
}
