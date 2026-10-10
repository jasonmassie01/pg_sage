package config

import (
	"strings"
	"testing"
)

// agents.query.*: the brokered read path's row and byte bounds (spec §9).

func TestAgentsQueryDefaults(t *testing.T) {
	c := DefaultConfig()
	if c.Agents.Query != (AgentsQueryConfig{MaxRows: 200, MaxRowsCeiling: 1000,
		MaxBytes: 1048576}) {
		t.Fatalf("query = %+v", c.Agents.Query)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
}

// Default value masking: setting one key keeps the others' defaults.
func TestAgentsQueryPartialYAMLKeepsDefaults(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  query:\n    max_rows: 50\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.Agents.Query.MaxRows != 50 || c.Agents.Query.MaxRowsCeiling != 1000 ||
		c.Agents.Query.MaxBytes != 1048576 {
		t.Fatalf("query = %+v", c.Agents.Query)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestAgentsQueryValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*AgentsQueryConfig)
		key    string
	}{
		"zero rows":     {func(q *AgentsQueryConfig) { q.MaxRows = 0 }, "agents.query.max_rows"},
		"zero ceiling":  {func(q *AgentsQueryConfig) { q.MaxRowsCeiling = 0 }, "max_rows_ceiling"},
		"rows > ceil":   {func(q *AgentsQueryConfig) { q.MaxRows = 1001 }, "max_rows_ceiling"},
		"zero bytes":    {func(q *AgentsQueryConfig) { q.MaxBytes = 0 }, "agents.query.max_bytes"},
		"neg bytes":     {func(q *AgentsQueryConfig) { q.MaxBytes = -1 }, "agents.query.max_bytes"},
		"huge ceiling":  {func(q *AgentsQueryConfig) { q.MaxRowsCeiling = 100001 }, "100000"},
		"huge max byte": {func(q *AgentsQueryConfig) { q.MaxBytes = 1<<30 + 1 }, "max_bytes"},
	}
	for name, c := range cases {
		cfg := DefaultConfig()
		c.mutate(&cfg.Agents.Query)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: Validate = %v, want an error naming %s", name, err, c.key)
		}
	}
	cfg := DefaultConfig()
	cfg.Agents.Query.MaxRows = cfg.Agents.Query.MaxRowsCeiling // boundary
	if err := cfg.Validate(); err != nil {
		t.Errorf("max_rows == ceiling rejected: %v", err)
	}
}
