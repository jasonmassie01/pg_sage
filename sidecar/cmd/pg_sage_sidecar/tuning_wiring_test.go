package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/tuning"
)

// The tuning agent's settings come from configuration (roadmap 2.2): its
// own budgets (tuning.*) and the existing switches, which now decide which
// proposal types the one agent may make.

func TestTuningSettingsFollowTheSwitches(t *testing.T) {
	c := config.DefaultConfig()
	s := tuningSettings(c, "rds", "app_db", 16<<30)
	for _, typ := range []tuning.ProposalType{tuning.ProposeIndexCreate,
		tuning.ProposeIndexDrop, tuning.ProposeGUC, tuning.ProposeReloption,
		tuning.ProposeStatistics, tuning.ProposeQueryHint} {
		if !s.Allowed[typ] {
			t.Errorf("%s is off by default", typ)
		}
	}
	if s.Tuning != c.Tuning || s.CloudEnv != "rds" || s.DatabaseName != "app_db" ||
		s.HostMemoryBytes != 16<<30 {
		t.Fatalf("settings = %+v", s)
	}
	if s.ConfidenceThreshold != c.LLM.Optimizer.ConfidenceThreshold ||
		s.Memory != c.LLM.Optimizer.RejectionMemory ||
		s.MaxOutputTokens != c.LLM.OptimizerLLM.MaxOutputTokens {
		t.Fatalf("optimizer-derived settings = %+v", s)
	}
	for _, tc := range []struct {
		name  string
		off   func(*config.Config)
		types []tuning.ProposalType
	}{
		{"llm.optimizer.enabled", func(c *config.Config) { c.LLM.Optimizer.Enabled = false },
			[]tuning.ProposalType{tuning.ProposeIndexCreate, tuning.ProposeIndexDrop}},
		{"advisor.memory_enabled", func(c *config.Config) { c.Advisor.MemoryEnabled = false },
			[]tuning.ProposalType{tuning.ProposeGUC}},
		{"advisor.vacuum_enabled", func(c *config.Config) { c.Advisor.VacuumEnabled = false },
			[]tuning.ProposalType{tuning.ProposeReloption}},
		{"advisor.enabled", func(c *config.Config) { c.Advisor.Enabled = false },
			[]tuning.ProposalType{tuning.ProposeGUC, tuning.ProposeReloption}},
		{"tuner.llm_enabled", func(c *config.Config) { c.Tuner.LLMEnabled = false },
			[]tuning.ProposalType{tuning.ProposeQueryHint}},
		{"tuner.enabled", func(c *config.Config) { c.Tuner.Enabled = false },
			[]tuning.ProposalType{tuning.ProposeQueryHint}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := config.DefaultConfig()
			tc.off(c)
			s := tuningSettings(c, "", "", 0)
			for _, typ := range tc.types {
				if s.Allowed[typ] {
					t.Errorf("%s=false still allows %s", tc.name, typ)
				}
			}
			if !s.Allowed[tuning.ProposeStatistics] {
				t.Error("extended statistics stay allowed")
			}
		})
	}
}
