package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestPostureConfig_MapsTheAgentsKeys(t *testing.T) {
	c := config.DefaultConfig()
	c.Agents.ExposedRoles = []string{"web_anon"}
	c.Agents.ClientPatterns = []string{"^my-agent"}
	c.Agents.Posture.MemoryGrowthGBDay = 2.5
	c.Agents.Posture.DailyAt = "22:15"
	p := postureConfig(c)
	if strings.Join(p.ExposedRoles, ",") != "web_anon" ||
		strings.Join(p.ClientPatterns, ",") != "^my-agent" ||
		p.MemoryGrowthGBDay != 2.5 || p.DailyAt != "22:15" {
		t.Fatalf("posture config = %+v", p)
	}
	// The mapped slices are copies: a hot reload must not race the run.
	c.Agents.ExposedRoles[0] = "changed"
	if p.ExposedRoles[0] != "web_anon" {
		t.Fatal("posture config shares the live config's slice")
	}
}

func TestPostureConfig_DefaultsMatchThePackageDefaults(t *testing.T) {
	got := postureConfig(config.DefaultConfig())
	want := agentposture.DefaultConfig()
	if strings.Join(got.ClientPatterns, ",") != strings.Join(want.ClientPatterns, ",") ||
		got.DailyAt != want.DailyAt || got.MemoryGrowthGBDay != want.MemoryGrowthGBDay ||
		len(got.ExposedRoles) != 0 {
		t.Fatalf("defaults differ: config %+v, package %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("default posture config invalid: %v", err)
	}
}

func TestPostureConfig_NilConfigUsesDefaults(t *testing.T) {
	got := postureConfig(nil)
	if got.DailyAt != "03:00" || len(got.ClientPatterns) != 6 {
		t.Fatalf("nil config = %+v", got)
	}
}

func TestFirstLookOptions_CarryThePostureConfig(t *testing.T) {
	c := config.DefaultConfig()
	c.Agents.ExposedRoles = []string{"web_anon"}
	opts := firstLookOptions(c, nil)
	if opts.Posture == nil || strings.Join(opts.Posture.ExposedRoles, ",") != "web_anon" {
		t.Fatalf("first look posture = %+v", opts.Posture)
	}
}
