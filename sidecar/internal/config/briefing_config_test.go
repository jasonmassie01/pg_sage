package config

import (
	"strings"
	"testing"
)

// llm-06: briefing.schedule, briefing.channels and briefing.slack_webhook_url
// are reconfigured live by the "briefing" owner; briefing.fleet_mode picks
// one roll-up message or one message per database in fleet mode.

// No concurrent access tests: these are pure lookups and load-time checks.

func TestBriefingFieldsAreReconfiguredByTheBriefingOwner(t *testing.T) {
	for _, path := range []string{"briefing.schedule", "briefing.channels",
		"briefing.slack_webhook_url"} {
		lc, ok := LookupFieldLifecycle(path)
		if !ok || lc.Lifecycle != LifecycleReconfigure || lc.Owner != BriefingOwner {
			t.Errorf("%s lifecycle = %+v (%v), want reconfigure by %q", path, lc, ok,
				BriefingOwner)
		}
	}
	// fleet_mode decides which goroutine dispatches, so it is restart-bound.
	lc, ok := LookupFieldLifecycle("briefing.fleet_mode")
	if !ok || lc.Lifecycle != LifecycleRestart || lc.Owner != "" {
		t.Fatalf("briefing.fleet_mode lifecycle = %+v (%v), want restart", lc, ok)
	}
	if BriefingOwner != "briefing" {
		t.Fatalf("BriefingOwner = %q, want \"briefing\"", BriefingOwner)
	}
}

// CHECK-14: the generated reference names briefing.* as reconfigure by an
// owner production registers.
func TestBriefingLifecycleReferenceNamesTheOwner(t *testing.T) {
	md := ConfigLifecycleMarkdown()
	for _, row := range []string{
		"| `briefing.schedule` | `reconfigure` | `briefing` |",
		"| `briefing.channels` | `reconfigure` | `briefing` |",
		"| `briefing.slack_webhook_url` | `reconfigure` | `briefing` |",
		"| `briefing.fleet_mode` | `restart` | `-` |",
	} {
		if !strings.Contains(md, row) {
			t.Errorf("lifecycle reference lacks %q", row)
		}
	}
	registered := false
	for _, name := range ProductionOwners() {
		registered = registered || name == BriefingOwner
	}
	if !registered {
		t.Fatal("ProductionOwners() does not include the briefing owner")
	}
}

// Default masking: without a file or a briefing section, fleet_mode is
// rollup.
func TestBriefingFleetModeDefaultsToRollup(t *testing.T) {
	if got := DefaultConfig().Briefing.FleetMode; got != BriefingFleetRollup {
		t.Fatalf("DefaultConfig briefing.fleet_mode = %q, want rollup", got)
	}
	if got := loadNoConfigFile(t).Briefing.FleetMode; got != BriefingFleetRollup {
		t.Fatalf("Load without a file: briefing.fleet_mode = %q, want rollup", got)
	}
	partial := loadYAMLConfig(t, "briefing:\n  schedule: \"0 7 * * *\"\n")
	if partial.Briefing.FleetMode != BriefingFleetRollup {
		t.Fatalf("partial briefing section: fleet_mode = %q, want rollup",
			partial.Briefing.FleetMode)
	}
	per := loadYAMLConfig(t, "briefing:\n  fleet_mode: per_database\n")
	if per.Briefing.FleetMode != BriefingFleetPerDatabase {
		t.Fatalf("fleet_mode: per_database loaded as %q", per.Briefing.FleetMode)
	}
}

// Invalid input: an unknown fleet_mode fails at load, naming the key.
func TestBriefingFleetModeRejectsUnknownValue(t *testing.T) {
	chdirTemp(t)
	path := writeYAMLFile(t, "briefing:\n  fleet_mode: everyone\n")
	_, err := Load([]string{"--config", path, "--pg-url", "postgres://u@localhost/db"})
	if err == nil || !strings.Contains(err.Error(), "briefing.fleet_mode") {
		t.Fatalf("fleet_mode: everyone = %v, want an error naming briefing.fleet_mode", err)
	}
	c := DefaultConfig()
	c.Briefing.FleetMode = ""
	if err := c.Briefing.validate(); err != nil {
		t.Fatalf("empty fleet_mode (means rollup) = %v, want nil", err)
	}
}

func TestBriefingFleetModeDocumented(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	doc := docs["briefing.fleet_mode"]
	if !strings.Contains(doc, "rollup") || !strings.Contains(doc, "per_database") {
		t.Fatalf("briefing.fleet_mode doc = %q, want both values named", doc)
	}
}
