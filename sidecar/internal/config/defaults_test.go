package config

import "testing"

// TestDefaultTokenBudgets_SaneForThinkingModels verifies that the
// default token budgets are large enough for thinking models
// (Gemini 2.5 series, o1/o3) which consume output tokens for
// internal reasoning. Bug 3 was caused by 4096 being too small.
func TestDefaultTokenBudgets_SaneForThinkingModels(t *testing.T) {
	// Context budget: minimum 8192 to leave room for thinking
	// tokens plus actual content in the response.
	if DefaultLLMContextBudget < 8192 {
		t.Errorf("DefaultLLMContextBudget = %d, want >= 8192 "+
			"(thinking models need headroom)",
			DefaultLLMContextBudget)
	}

	// Optimizer max output tokens: same minimum.
	if DefaultOptLLMMaxOutputTokens < 8192 {
		t.Errorf("DefaultOptLLMMaxOutputTokens = %d, "+
			"want >= 8192", DefaultOptLLMMaxOutputTokens)
	}

	// Daily token budget should be non-trivial (at least 100k)
	// to avoid immediate exhaustion in production.
	if DefaultLLMTokenBudget < 100000 {
		t.Errorf("DefaultLLMTokenBudget = %d, want >= 100000",
			DefaultLLMTokenBudget)
	}
	if DefaultOptLLMTokenBudget < 100000 {
		t.Errorf("DefaultOptLLMTokenBudget = %d, "+
			"want >= 100000", DefaultOptLLMTokenBudget)
	}
}

// TestDefaultConfig_PopulatesLLMDefaults verifies that NewDefault
// sets all LLM-related fields to their expected non-zero defaults.
func TestDefaultConfig_PopulatesLLMDefaults(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.LLM.TimeoutSeconds != DefaultLLMTimeoutSeconds {
		t.Errorf("LLM.TimeoutSeconds = %d, want %d",
			cfg.LLM.TimeoutSeconds, DefaultLLMTimeoutSeconds)
	}
	if cfg.LLM.TokenBudgetDaily != DefaultLLMTokenBudget {
		t.Errorf("LLM.TokenBudgetDaily = %d, want %d",
			cfg.LLM.TokenBudgetDaily, DefaultLLMTokenBudget)
	}
	if cfg.LLM.ContextBudgetTokens != DefaultLLMContextBudget {
		t.Errorf("LLM.ContextBudgetTokens = %d, want %d",
			cfg.LLM.ContextBudgetTokens, DefaultLLMContextBudget)
	}
	if cfg.LLM.CooldownSeconds != DefaultLLMCooldownSeconds {
		t.Errorf("LLM.CooldownSeconds = %d, want %d",
			cfg.LLM.CooldownSeconds, DefaultLLMCooldownSeconds)
	}

	// Optimizer LLM defaults.
	optLLM := cfg.LLM.OptimizerLLM
	if optLLM.TimeoutSeconds != DefaultOptLLMTimeoutSeconds {
		t.Errorf("OptimizerLLM.TimeoutSeconds = %d, want %d",
			optLLM.TimeoutSeconds, DefaultOptLLMTimeoutSeconds)
	}
	if optLLM.TokenBudgetDaily != DefaultOptLLMTokenBudget {
		t.Errorf("OptimizerLLM.TokenBudgetDaily = %d, want %d",
			optLLM.TokenBudgetDaily, DefaultOptLLMTokenBudget)
	}
	if optLLM.MaxOutputTokens != DefaultOptLLMMaxOutputTokens {
		t.Errorf("OptimizerLLM.MaxOutputTokens = %d, want %d",
			optLLM.MaxOutputTokens, DefaultOptLLMMaxOutputTokens)
	}
	if !optLLM.FallbackToGeneral {
		t.Error("OptimizerLLM.FallbackToGeneral should default " +
			"to true")
	}
}

// TestDefaultConfig_CriticalNonZeroDefaults verifies that commonly
// misset defaults (those that caused bugs when zero) are populated.
func TestDefaultConfig_CriticalNonZeroDefaults(t *testing.T) {
	cfg := DefaultConfig()

	checks := []struct {
		name string
		got  int
		want int
	}{
		{"UnusedIndexWindowDays",
			cfg.Analyzer.UnusedIndexWindowDays,
			DefaultUnusedIndexWindowDays},
		{"SlowQueryThresholdMs",
			cfg.Analyzer.SlowQueryThresholdMs,
			DefaultSlowQueryThresholdMs},
		{"SeqScanMinRows",
			cfg.Analyzer.SeqScanMinRows,
			DefaultSeqScanMinRows},
		{"CollectorBatchSize",
			cfg.Collector.BatchSize,
			DefaultCollectorBatchSize},
		{"DDLTimeoutSeconds",
			cfg.Safety.DDLTimeoutSeconds,
			DefaultDDLTimeoutSeconds},
		{"LockTimeoutMs",
			cfg.Safety.LockTimeoutMs,
			DefaultLockTimeoutMs},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
		if c.got == 0 {
			t.Errorf("%s should never be zero (causes bugs "+
				"when used as denominator or threshold)",
				c.name)
		}
	}
}

// TestDefaultConfig_TunerV085Defaults verifies that the v0.8.5
// tuner fields (hint revalidation, stale stats) have sane non-zero
// defaults. These are production-critical: zero values for ratios
// and thresholds would cause always-retire or never-retire behavior.
func TestDefaultConfig_TunerV085Defaults(t *testing.T) {
	cfg := DefaultConfig()
	tu := cfg.Tuner

	if tu.HintRetirementDays != DefaultTunerHintRetirementDays {
		t.Errorf("HintRetirementDays = %d, want %d",
			tu.HintRetirementDays, DefaultTunerHintRetirementDays)
	}
	if tu.HintRetirementDays == 0 {
		t.Error("HintRetirementDays must not be 0 " +
			"(would retire hints immediately)")
	}

	if tu.RevalidationIntervalHours !=
		DefaultTunerRevalidationIntervalHours {
		t.Errorf("RevalidationIntervalHours = %d, want %d",
			tu.RevalidationIntervalHours,
			DefaultTunerRevalidationIntervalHours)
	}

	if tu.RevalidationKeepRatio !=
		DefaultTunerRevalidationKeepRatio {
		t.Errorf("RevalidationKeepRatio = %f, want %f",
			tu.RevalidationKeepRatio,
			DefaultTunerRevalidationKeepRatio)
	}
	if tu.RevalidationKeepRatio <= 0 {
		t.Error("RevalidationKeepRatio must be > 0")
	}

	if tu.RevalidationRollbackRatio !=
		DefaultTunerRevalidationRollbackRatio {
		t.Errorf("RevalidationRollbackRatio = %f, want %f",
			tu.RevalidationRollbackRatio,
			DefaultTunerRevalidationRollbackRatio)
	}

	if tu.RevalidationExplainTimeoutMs !=
		DefaultTunerRevalidationExplainTimeoutMs {
		t.Errorf("RevalidationExplainTimeoutMs = %d, want %d",
			tu.RevalidationExplainTimeoutMs,
			DefaultTunerRevalidationExplainTimeoutMs)
	}

	if tu.StaleStatsEstimateSkew !=
		DefaultTunerStaleStatsEstimateSkew {
		t.Errorf("StaleStatsEstimateSkew = %f, want %f",
			tu.StaleStatsEstimateSkew,
			DefaultTunerStaleStatsEstimateSkew)
	}
	if tu.StaleStatsEstimateSkew <= 0 {
		t.Error("StaleStatsEstimateSkew must be > 0 " +
			"(zero would flag every plan node)")
	}

	if tu.StaleStatsModRatio != DefaultTunerStaleStatsModRatio {
		t.Errorf("StaleStatsModRatio = %f, want %f",
			tu.StaleStatsModRatio, DefaultTunerStaleStatsModRatio)
	}

	if tu.StaleStatsAgeMinutes !=
		DefaultTunerStaleStatsAgeMinutes {
		t.Errorf("StaleStatsAgeMinutes = %d, want %d",
			tu.StaleStatsAgeMinutes,
			DefaultTunerStaleStatsAgeMinutes)
	}

	if tu.AnalyzeMaxTableMB != DefaultTunerAnalyzeMaxTableMB {
		t.Errorf("AnalyzeMaxTableMB = %d, want %d",
			tu.AnalyzeMaxTableMB, DefaultTunerAnalyzeMaxTableMB)
	}

	if tu.AnalyzeCooldownMinutes !=
		DefaultTunerAnalyzeCooldownMinutes {
		t.Errorf("AnalyzeCooldownMinutes = %d, want %d",
			tu.AnalyzeCooldownMinutes,
			DefaultTunerAnalyzeCooldownMinutes)
	}

	if tu.AnalyzeTimeoutMs != DefaultTunerAnalyzeTimeoutMs {
		t.Errorf("AnalyzeTimeoutMs = %d, want %d",
			tu.AnalyzeTimeoutMs, DefaultTunerAnalyzeTimeoutMs)
	}
	if tu.AnalyzeTimeoutMs == 0 {
		t.Error("AnalyzeTimeoutMs must not be 0 " +
			"(would cause immediate timeout)")
	}

	if tu.MaxConcurrentAnalyze !=
		DefaultTunerMaxConcurrentAnalyze {
		t.Errorf("MaxConcurrentAnalyze = %d, want %d",
			tu.MaxConcurrentAnalyze,
			DefaultTunerMaxConcurrentAnalyze)
	}
}

// TestDefaultConfig_OptimizerLLMTimeoutAdequate verifies the optimizer
// LLM timeout is high enough for thinking models that may take 60-120s
// to produce a response with internal reasoning.
func TestDefaultConfig_OptimizerLLMTimeoutAdequate(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.LLM.OptimizerLLM.TimeoutSeconds < 60 {
		t.Errorf("OptimizerLLM.TimeoutSeconds = %d, want >= 60 "+
			"(thinking models need longer timeouts)",
			cfg.LLM.OptimizerLLM.TimeoutSeconds)
	}
}

// TestDefaultConfig_AnalyzerWorkMemPromotion verifies the v0.8.5
// work_mem promotion threshold has a sane default.
func TestDefaultConfig_AnalyzerWorkMemPromotion(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Analyzer.WorkMemPromotionThreshold !=
		DefaultAnalyzerWorkMemPromotionThreshold {
		t.Errorf("WorkMemPromotionThreshold = %d, want %d",
			cfg.Analyzer.WorkMemPromotionThreshold,
			DefaultAnalyzerWorkMemPromotionThreshold)
	}
}

// TestDefaultConfig_TrustLevel verifies the trust level defaults
// to the safest value.
func TestDefaultConfig_TrustLevel(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Trust.Level != "observation" {
		t.Errorf("Trust.Level = %q, want %q (safest default)",
			cfg.Trust.Level, "observation")
	}
}
