package agentposture

import (
	"errors"
	"testing"
)

func TestDefaultConfig_MatchesTheSpec(t *testing.T) {
	c := DefaultConfig()
	want := []string{"^mcp", "^claude", "^cursor", "^codex", "^langgraph", "^crewai"}
	if len(c.ClientPatterns) != len(want) {
		t.Fatalf("client patterns = %v, want %v", c.ClientPatterns, want)
	}
	for i := range want {
		if c.ClientPatterns[i] != want[i] {
			t.Fatalf("client patterns = %v, want %v", c.ClientPatterns, want)
		}
	}
	if len(c.ExposedRoles) != 0 || c.MemoryGrowthGBDay != 5 || c.DailyAt != "03:00" {
		t.Fatalf("defaults = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
}

func TestConfigValidate_Rejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"unanchored pattern", func(c *Config) { c.ClientPatterns = []string{"mcp"} }},
		{"bad regexp", func(c *Config) { c.ClientPatterns = []string{"^mcp("} }},
		{"empty pattern", func(c *Config) { c.ClientPatterns = []string{""} }},
		{"empty exposed role", func(c *Config) { c.ExposedRoles = []string{""} }},
		{"public as exposed role", func(c *Config) { c.ExposedRoles = []string{"PUBLIC"} }},
		{"negative growth", func(c *Config) { c.MemoryGrowthGBDay = -1 }},
		{"zero growth", func(c *Config) { c.MemoryGrowthGBDay = 0 }},
		{"daily_at 24:00", func(c *Config) { c.DailyAt = "24:00" }},
		{"daily_at 3am", func(c *Config) { c.DailyAt = "3am" }},
		{"daily_at minutes", func(c *Config) { c.DailyAt = "03:60" }},
		{"daily_at empty", func(c *Config) { c.DailyAt = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mut(&c)
			if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConfigValidate_AcceptsEmptyPatternsAndBoundaries(t *testing.T) {
	c := DefaultConfig()
	c.ClientPatterns = nil
	c.ExposedRoles = []string{"web_anon"}
	for _, at := range []string{"00:00", "23:59", "3:05"} {
		c.DailyAt = at
		if err := c.Validate(); err != nil {
			t.Fatalf("daily_at %q: %v", at, err)
		}
	}
}

func TestConfig_MatchClient(t *testing.T) {
	c := DefaultConfig()
	cases := map[string]bool{
		"mcp-server-postgres": true,
		"Claude Code":         true, // case-insensitive
		"cursor":              true,
		"langgraph-worker":    true,
		"my-mcp-app":          false, // anchored: a hint, not a substring search
		"psql":                false,
		"":                    false,
		"pg_sage":             false,
	}
	for app, want := range cases {
		if got := c.MatchClient(app); got != want {
			t.Errorf("MatchClient(%q) = %v, want %v", app, got, want)
		}
	}
	bad := Config{ClientPatterns: []string{"^mcp("}}
	if bad.MatchClient("mcp") {
		t.Fatal("an invalid pattern matched")
	}
}
