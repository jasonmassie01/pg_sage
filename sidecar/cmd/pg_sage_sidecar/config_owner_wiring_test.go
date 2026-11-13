package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// modeConfig is a process config for one sidecar mode. Meta mode is
// recognised by meta_db (HasMetaDB), as in initModeRuntime.
func modeConfig(mode string) *config.Config {
	c := config.DefaultConfig()
	c.LLM.Enabled = false
	switch mode {
	case "meta":
		c.Mode, c.MetaDB = "fleet", "postgres://meta.invalid/sage"
	default:
		c.Mode = mode
	}
	return c
}

// registerOwnersForTest runs the registration helpers a mode's startup
// runs, in startup order, without connecting to any database.
func registerOwnersForTest(t *testing.T, mode string) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	oldPool, oldMeta := pool, globalMetaState
	t.Cleanup(func() { pool, globalMetaState = oldPool, oldMeta })
	pool, globalMetaState, configController = nil, nil, nil
	cfg = modeConfig(mode)
	if err := initializeConfigController(nil); err != nil {
		t.Fatalf("%s: initialize config controller: %v", mode, err)
	}
	llmClient = llm.New(&cfg.LLM, nil)
	registerLLMConfigOwner()
	registerFleetDatabasesOwner(&fleetBootstrap{})
}

// requiredOwners is every owner a field that applies in the mode names.
// The YAML fleet's databases and defaults exist only in fleet mode.
func requiredOwners(c *config.Config) []string {
	fields := config.FieldLifecycles()
	if ownsYAMLFleet(c) {
		fields = append(fields, config.DatabaseFieldLifecycles()...)
	}
	set := map[string]bool{}
	for _, field := range fields {
		if field.Owner == "" ||
			(field.Owner == config.FleetDatabasesOwner && !ownsYAMLFleet(c)) {
			continue
		}
		set[field.Owner] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ownsYAMLFleet(c *config.Config) bool { return c.IsFleet() && !c.HasMetaDB() }

func TestEveryModeRegistersTheOwnersItsFieldsName(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{"standalone", "trust_policy,llm,llm_redaction,briefing,alerting"},
		{"fleet", "trust_policy,llm,llm_redaction,briefing,alerting,fleet_databases"},
		{"meta", "trust_policy,llm,llm_redaction,briefing,alerting"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			registerOwnersForTest(t, tt.mode)
			got := configController.OwnerNames()
			if strings.Join(got, ",") != tt.want {
				t.Fatalf("OwnerNames() = %v, want %s", got, tt.want)
			}
			registered := map[string]bool{}
			for _, name := range got {
				registered[name] = true
			}
			for _, owner := range requiredOwners(cfg) {
				if !registered[owner] {
					t.Errorf("a %s field names owner %q, which %s never registers",
						tt.mode, owner, tt.mode)
				}
			}
			if strings.Join(ownersForMode(cfg), ",") != tt.want {
				t.Fatalf("ownersForMode = %v, want %s", ownersForMode(cfg), tt.want)
			}
		})
	}
}

// Every owner a mode expects is a production owner the metadata may name.
func TestOwnersForModeAreProductionOwners(t *testing.T) {
	production := map[string]bool{}
	for _, name := range config.ProductionOwners() {
		production[name] = true
	}
	for _, mode := range []string{"standalone", "fleet", "meta", "extension"} {
		for _, name := range ownersForMode(modeConfig(mode)) {
			if !production[name] {
				t.Errorf("%s expects owner %q, which is not a production owner", mode, name)
			}
		}
	}
	got := strings.Join(ownersForMode(nil), ",")
	if got != "trust_policy,llm,llm_redaction,briefing,alerting" {
		t.Fatalf("ownersForMode(nil) = %v, want the process owners", got)
	}
}

// Nil: the helpers are no-ops before a controller exists.
func TestOwnerHelpersWithoutControllerDoNothing(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	configController = nil
	cfg = modeConfig("fleet")
	llmClient = llm.New(&cfg.LLM, nil)
	registerLLMConfigOwner()
	registerFleetDatabasesOwner(&fleetBootstrap{})
	if configController != nil {
		t.Fatal("a helper created a controller")
	}
}

// Boundary: a second run of the helpers never duplicates an owner.
func TestOwnerHelpersRunTwiceKeepOneOwnerEach(t *testing.T) {
	registerOwnersForTest(t, "fleet")
	registerLLMConfigOwner()
	registerFleetDatabasesOwner(&fleetBootstrap{})
	if got := strings.Join(configController.OwnerNames(), ","); got !=
		"trust_policy,llm,llm_redaction,briefing,alerting,fleet_databases" {
		t.Fatalf("OwnerNames() after a second registration = %s", got)
	}
}
