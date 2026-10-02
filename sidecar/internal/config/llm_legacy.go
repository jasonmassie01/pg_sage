package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// optimizerEnableKeys records which optimizer enable keys a YAML file
// actually sets; a nil pointer means the key is absent.
type optimizerEnableKeys struct {
	LLM struct {
		IndexOptimizer struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"index_optimizer"`
		Optimizer struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"optimizer"`
	} `yaml:"llm"`
}

// migrateLegacyIndexOptimizer applies the deprecated llm.index_optimizer
// block to llm.optimizer, but only when the file sets
// llm.index_optimizer.enabled and leaves llm.optimizer.enabled unset: the
// current key always wins, and an explicit legacy false is honoured now
// that llm.optimizer.enabled defaults to true. Comparing values instead
// of keys would let a default override an explicit setting.
func migrateLegacyIndexOptimizer(raw string, cfg *Config) error {
	var keys optimizerEnableKeys
	if err := yaml.Unmarshal([]byte(raw), &keys); err != nil {
		return fmt.Errorf("read llm.index_optimizer: %w", err)
	}
	legacy := keys.LLM.IndexOptimizer.Enabled
	if legacy == nil || keys.LLM.Optimizer.Enabled != nil {
		return nil
	}
	cfg.LLM.Optimizer.Enabled = *legacy
	if *legacy {
		copyLegacyOptimizerTuning(&cfg.LLM)
	}
	return nil
}

// copyLegacyOptimizerTuning carries the positive legacy tuning values over.
func copyLegacyOptimizerTuning(l *LLMConfig) {
	legacy, opt := l.IndexOptimizer, &l.Optimizer
	if legacy.MinQueryCalls > 0 {
		opt.MinQueryCalls = legacy.MinQueryCalls
	}
	if legacy.MaxIndexesPerTable > 0 {
		opt.MaxIndexesPerTable = legacy.MaxIndexesPerTable
	}
	if legacy.MaxIncludeColumns > 0 {
		opt.MaxIncludeColumns = legacy.MaxIncludeColumns
	}
	if legacy.OverIndexedRatio > 0 {
		opt.OverIndexedRatioPct = legacy.OverIndexedRatio
	}
	if legacy.WriteHeavyRatio > 0 {
		opt.WriteHeavyRatioPct = legacy.WriteHeavyRatio
	}
}
