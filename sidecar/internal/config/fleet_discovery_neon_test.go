package config

import (
	"strings"
	"testing"
)

// fleetpol-01b: neon discovery sources. No concurrent access tests:
// Validate is a pure function of the value.
func TestDiscoverySource_Neon(t *testing.T) {
	base := FleetDiscoveryConfig{Enabled: true, MaxEnrolled: 5, TTLHours: 24}
	for _, tc := range []struct {
		name string
		src  DiscoverySourceConfig
		want string // "" valid
	}{
		{"every project", DiscoverySourceConfig{Name: "neon", Type: "neon", Owner: "t"}, ""},
		{"one project and database", DiscoverySourceConfig{Name: "neon", Type: "neon",
			Owner: "t", Project: "p-one", Database: "app"}, ""},
		{"no owner", DiscoverySourceConfig{Name: "neon", Type: "neon"}, "owner"},
		{"reuse credentials", DiscoverySourceConfig{Name: "neon", Type: "neon", Owner: "t",
			ReuseCredentials: true}, "cluster sources only"},
		{"database on rds", DiscoverySourceConfig{Name: "r", Type: "rds", Owner: "t",
			Region: "us-east-1", TagKey: "k", Database: "x"}, "neon sources only"},
		{"unknown type", DiscoverySourceConfig{Name: "x", Type: "supabase", Owner: "t"},
			"cluster, lakebase, rds or neon"},
	} {
		cfg := base
		cfg.Sources = []DiscoverySourceConfig{tc.src}
		err := cfg.Validate()
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}
