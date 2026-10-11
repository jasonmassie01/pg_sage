package config

import (
	"strings"
	"testing"
)

// agents.*: the agent posture keys of G0 (spec §9). Posture runs in every
// first look and daily; these keys only shape what it checks.

func TestAgentsDefaults(t *testing.T) {
	a := DefaultConfig().Agents
	want := []string{"^mcp", "^claude", "^cursor", "^codex", "^langgraph", "^crewai"}
	if strings.Join(a.ClientPatterns, ",") != strings.Join(want, ",") {
		t.Fatalf("client_patterns = %v, want %v", a.ClientPatterns, want)
	}
	if len(a.ExposedRoles) != 0 {
		t.Fatalf("exposed_roles = %v, want none (PUBLIC is always exposed)", a.ExposedRoles)
	}
	if a.Posture.MemoryGrowthGBDay != 5 || a.Posture.DailyAt != "03:00" {
		t.Fatalf("posture = %+v", a.Posture)
	}
}

// Default value masking: with no agents key at all, the defaults hold.
func TestAgentsDefaultsSurviveAConfigWithoutAgents(t *testing.T) {
	path := wave5WriteYAML(t, "mode: standalone\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if len(c.Agents.ClientPatterns) != 6 || c.Agents.Posture.DailyAt != "03:00" ||
		c.Agents.Posture.MemoryGrowthGBDay != 5 {
		t.Fatalf("agents = %+v", c.Agents)
	}
}

func TestAgentsLoadsFromYAML(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  exposed_roles: [web_anon]\n"+
		"  client_patterns: ['^my-agent']\n  posture:\n    daily_at: \"22:30\"\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	a := c.Agents
	if strings.Join(a.ExposedRoles, ",") != "web_anon" ||
		strings.Join(a.ClientPatterns, ",") != "^my-agent" || a.Posture.DailyAt != "22:30" {
		t.Fatalf("loaded %+v", a)
	}
	if a.Posture.MemoryGrowthGBDay != 5 {
		t.Fatalf("unset key lost its default: %+v", a.Posture)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid agents config rejected: %v", err)
	}
}

func TestAgentsEmptyClientPatternsDisableHints(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  client_patterns: []\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if len(c.Agents.ClientPatterns) != 0 {
		t.Fatalf("client_patterns = %v, want none", c.Agents.ClientPatterns)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("empty patterns rejected: %v", err)
	}
}

func TestAgentsValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AgentsConfig)
		key  string
	}{
		{"unanchored pattern", func(a *AgentsConfig) { a.ClientPatterns = []string{"mcp"} },
			"agents.client_patterns"},
		{"bad regexp", func(a *AgentsConfig) { a.ClientPatterns = []string{"^mcp["} },
			"agents.client_patterns"},
		{"empty role", func(a *AgentsConfig) { a.ExposedRoles = []string{" "} },
			"agents.exposed_roles"},
		{"public role", func(a *AgentsConfig) { a.ExposedRoles = []string{"public"} },
			"agents.exposed_roles"},
		{"growth zero", func(a *AgentsConfig) { a.Posture.MemoryGrowthGBDay = 0 },
			"agents.posture.memory_growth_gb_day"},
		{"growth negative", func(a *AgentsConfig) { a.Posture.MemoryGrowthGBDay = -2 },
			"agents.posture.memory_growth_gb_day"},
		{"daily_at hour", func(a *AgentsConfig) { a.Posture.DailyAt = "25:00" },
			"agents.posture.daily_at"},
		{"daily_at text", func(a *AgentsConfig) { a.Posture.DailyAt = "nightly" },
			"agents.posture.daily_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mut(&c.Agents)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err = %v, want one naming %s", err, tc.key)
			}
		})
	}
}

func TestParseDailyAt(t *testing.T) {
	ok := map[string][2]int{"00:00": {0, 0}, "03:00": {3, 0}, "23:59": {23, 59},
		"7:05": {7, 5}}
	for s, want := range ok {
		h, m, err := ParseDailyAt(s)
		if err != nil || h != want[0] || m != want[1] {
			t.Errorf("ParseDailyAt(%q) = %d, %d, %v", s, h, m, err)
		}
	}
	for _, s := range []string{"", "24:00", "12:60", "12", "12:5", "-1:00", "ab:cd",
		"12:00:00", " 3:00"} {
		if _, _, err := ParseDailyAt(s); err == nil {
			t.Errorf("ParseDailyAt(%q) accepted", s)
		}
	}
}

func TestValidClientPattern(t *testing.T) {
	for _, p := range []string{"^mcp", "^(claude|cursor)", "^langgraph-.*$"} {
		if err := ValidClientPattern(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"", "mcp", ".*mcp", "^mcp(", "(?i)mcp"} {
		if err := ValidClientPattern(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestAgentsKeyClasses(t *testing.T) {
	cases := map[string]KeyClass{
		"agents.exposed_roles":                KeySafetyCritical,
		"agents.client_patterns":              KeyOperatorPreference,
		"agents.posture.memory_growth_gb_day": KeyOperatorPreference,
		"agents.posture.daily_at":             KeyOperatorPreference,
	}
	for key, want := range cases {
		class, ok := KeyClassOf(key)
		if !ok || class != want {
			t.Errorf("%s class = %q (known=%v), want %s", key, class, ok, want)
		}
	}
}
