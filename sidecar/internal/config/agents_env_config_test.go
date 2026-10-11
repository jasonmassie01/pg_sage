package config

import (
	"strings"
	"testing"
)

// agents.control_database and agents.default_environment (spec §6.5, §9):
// governance state needs meta mode or a pinned control database, and an
// unverified environment is always prod, so prod is the only default.

func TestAgentsEnvDefaults(t *testing.T) {
	a := DefaultConfig().Agents
	if a.ControlDatabase != "" || a.DefaultEnvironment != "prod" {
		t.Fatalf("defaults: control %q environment %q", a.ControlDatabase, a.DefaultEnvironment)
	}
	path := wave5WriteYAML(t, "mode: standalone\nagents:\n  exposed_roles: [web]\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.DefaultEnvironment != "prod" {
		t.Fatalf("a YAML agents block without the key keeps prod: %q",
			c.Agents.DefaultEnvironment)
	}
}

func TestAgentsEnvLoadsAndValidates(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  control_database: governance\n"+
		"  default_environment: prod\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.ControlDatabase != "governance" {
		t.Fatalf("control_database %q", c.Agents.ControlDatabase)
	}
	if err := c.Agents.validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	cases := map[string]func(*AgentsConfig){
		"dev default":     func(a *AgentsConfig) { a.DefaultEnvironment = "dev" },
		"unknown env":     func(a *AgentsConfig) { a.DefaultEnvironment = "production" },
		"blank env":       func(a *AgentsConfig) { a.DefaultEnvironment = "" },
		"spaces":          func(a *AgentsConfig) { a.ControlDatabase = " governance" },
		"control char":    func(a *AgentsConfig) { a.ControlDatabase = "gov\nx" },
		"too long":        func(a *AgentsConfig) { a.ControlDatabase = strings.Repeat("g", 129) },
		"whitespace only": func(a *AgentsConfig) { a.ControlDatabase = "  " },
	}
	for name, mut := range cases {
		a := DefaultConfig().Agents
		mut(&a)
		err := a.validate()
		if err == nil || !strings.Contains(err.Error(), "agents.") {
			t.Errorf("%s: %v", name, err)
		}
	}
	a := DefaultConfig().Agents
	a.DefaultEnvironment = "dev"
	if err := a.validate(); err == nil || !strings.Contains(err.Error(), "prod") {
		t.Errorf("the refusal names the only allowed value: %v", err)
	}
}
