package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The handler asks for the provider of the database it resolved, so a
// fleet member's U7 dates are its own provider's.
func TestUpgradeReadiness_ProviderIsTheRequestedDatabases(t *testing.T) {
	for query, want := range map[string]string{
		"database=prod&target_major=18": "rds",
		"target_major=18":               "aurora",
	} {
		f := &fakeUpgrade{}
		if code, body := getUpgrade(t, upgradeDeps(f), query); code != 200 {
			t.Fatalf("%s = %d %s", query, code, body)
		}
		if f.target.Provider != want {
			t.Fatalf("%s ran with provider %q, want %q", query, f.target.Provider, want)
		}
	}
}

func providerFleet(platforms map[string]string) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for name, platform := range platforms {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Platform: platform}})
	}
	return mgr
}

func TestMemberProvider(t *testing.T) {
	cfg := &config.Config{CloudEnvironment: "self-managed"}
	mgr := providerFleet(map[string]string{"orders": "rds", "events": "aurora",
		"legacy": ""})
	cases := []struct {
		mgr      *fleet.DatabaseManager
		cfg      *config.Config
		database string
		want     string
	}{
		{mgr, cfg, "orders", "rds"},
		{mgr, cfg, "events", "aurora"},
		{mgr, cfg, "legacy", "self-managed"}, // the member reports none
		{mgr, cfg, "ghost", "self-managed"},
		{mgr, cfg, "", "self-managed"}, // several members: no single one
		{providerFleet(map[string]string{"only": "aurora"}), cfg, "", "aurora"},
		{nil, cfg, "orders", "self-managed"},
		{nil, nil, "orders", ""},
	}
	for i, c := range cases {
		if got := memberProvider(c.mgr, c.cfg, c.database); got != c.want {
			t.Fatalf("case %d (%q) = %q, want %q", i, c.database, got, c.want)
		}
	}
}
