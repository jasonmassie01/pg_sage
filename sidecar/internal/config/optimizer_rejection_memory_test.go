package config

import (
	"strings"
	"testing"
)

// llm.optimizer.rejection_memory: the optimizer remembers HypoPG what-if
// rejections and does not re-measure the same idea until the workload or
// table changes materially or the memory ages out.

// No concurrent access tests: configuration values are plain data.

func TestRejectionMemory_DefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := OptimizerRejectionMemoryConfig{Enabled: true, MaxAgeDays: 7, CallVolumeRatio: 2,
		MeanTimeRatio: 2, RowEstimateRatio: 2, PromptMaxShapes: 5}
	if got := cfg.LLM.Optimizer.RejectionMemory; got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	if DefaultConfig().LLM.Optimizer.RejectionMemory != want ||
		DefaultOptimizerRejectionMemory() != want {
		t.Fatal("DefaultConfig / DefaultOptimizerRejectionMemory disagree with Load(nil)")
	}
}

func TestRejectionMemory_PartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t,
		"llm:\n  optimizer:\n    rejection_memory:\n      max_age_days: 3\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.LLM.Optimizer.RejectionMemory
	if got.MaxAgeDays != 3 || !got.Enabled || got.CallVolumeRatio != 2 ||
		got.MeanTimeRatio != 2 || got.RowEstimateRatio != 2 || got.PromptMaxShapes != 5 {
		t.Fatalf("partial section = %+v, want max_age_days 3 and every other default", got)
	}
	if !cfg.LLM.Optimizer.Enabled || cfg.LLM.Optimizer.HypoPGMinImprovePct != 10 {
		t.Fatal("the rejection_memory block reset sibling optimizer defaults")
	}
}

func TestRejectionMemory_ExplicitValuesAndDisable(t *testing.T) {
	cfg, err := loadRCAYAML(t, "llm:\n  optimizer:\n    rejection_memory:\n"+
		"      enabled: false\n      max_age_days: 90\n      call_volume_ratio: 1.5\n"+
		"      mean_time_ratio: 1000\n      row_estimate_ratio: 3\n"+
		"      prompt_max_shapes: 20\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := OptimizerRejectionMemoryConfig{Enabled: false, MaxAgeDays: 90,
		CallVolumeRatio: 1.5, MeanTimeRatio: 1000, RowEstimateRatio: 3, PromptMaxShapes: 20}
	if got := cfg.LLM.Optimizer.RejectionMemory; got != want {
		t.Fatalf("explicit = %+v, want %+v", got, want)
	}
}

func TestRejectionMemory_BoundaryValuesAccepted(t *testing.T) {
	for _, line := range []string{"max_age_days: 1", "max_age_days: 90",
		"call_volume_ratio: 1.01", "mean_time_ratio: 1000", "row_estimate_ratio: 1.001",
		"prompt_max_shapes: 1", "prompt_max_shapes: 20"} {
		if _, err := loadRCAYAML(t, "llm:\n  optimizer:\n    rejection_memory:\n      "+
			line+"\n"); err != nil {
			t.Errorf("%s refused: %v", line, err)
		}
	}
}

func TestRejectionMemory_OutOfRangeRefused(t *testing.T) {
	cases := map[string]string{
		"max_age_days: 0":          "llm.optimizer.rejection_memory.max_age_days",
		"max_age_days: 91":         "llm.optimizer.rejection_memory.max_age_days",
		"max_age_days: -1":         "llm.optimizer.rejection_memory.max_age_days",
		"call_volume_ratio: 1":     "llm.optimizer.rejection_memory.call_volume_ratio",
		"call_volume_ratio: 0.5":   "llm.optimizer.rejection_memory.call_volume_ratio",
		"call_volume_ratio: .nan":  "llm.optimizer.rejection_memory.call_volume_ratio",
		"mean_time_ratio: 1000.5":  "llm.optimizer.rejection_memory.mean_time_ratio",
		"mean_time_ratio: 0":       "llm.optimizer.rejection_memory.mean_time_ratio",
		"row_estimate_ratio: -2":   "llm.optimizer.rejection_memory.row_estimate_ratio",
		"row_estimate_ratio: .inf": "llm.optimizer.rejection_memory.row_estimate_ratio",
		"prompt_max_shapes: 0":     "llm.optimizer.rejection_memory.prompt_max_shapes",
		"prompt_max_shapes: 21":    "llm.optimizer.rejection_memory.prompt_max_shapes",
	}
	for line, key := range cases {
		_, err := loadRCAYAML(t, "llm:\n  optimizer:\n    rejection_memory:\n      "+
			line+"\n")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v, want a refusal naming %s", line, err, key)
		}
	}
}

func TestRejectionMemory_UnknownKeyRefused(t *testing.T) {
	_, err := loadRCAYAML(t, "llm:\n  optimizer:\n    rejection_memory:\n"+
		"      max_age_hours: 5\n")
	if err == nil || !strings.Contains(err.Error(), "max_age_hours") {
		t.Fatalf("unknown key accepted: %v", err)
	}
}

func TestRejectionMemory_LifecycleIsRegistered(t *testing.T) {
	for _, key := range []string{"enabled", "max_age_days", "call_volume_ratio",
		"mean_time_ratio", "row_estimate_ratio", "prompt_max_shapes"} {
		path := "llm.optimizer.rejection_memory." + key
		if _, ok := LookupFieldLifecycle(path); !ok {
			t.Errorf("%s is not in the lifecycle registry", path)
		}
	}
}
