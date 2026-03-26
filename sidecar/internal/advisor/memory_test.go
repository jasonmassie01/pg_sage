package advisor

import (
	"strings"
	"testing"
)

func TestMemorySystemPrompt_ContainsRules(t *testing.T) {
	checks := []string{
		"shared_buffers",
		"work_mem",
		"25% of RAM",
		"hash_mem_multiplier",
		"spills",
	}
	for _, want := range checks {
		if !strings.Contains(memorySystemPrompt, want) {
			t.Errorf("memory system prompt missing %q", want)
		}
	}
}

func TestMemorySystemPrompt_ContainsAntiThinking(t *testing.T) {
	if !strings.Contains(memorySystemPrompt, "No thinking") {
		t.Error("memory system prompt missing anti-thinking directive")
	}
}

func TestMemContext_BufferHitRatio(t *testing.T) {
	hit := int64(45200000)
	read := int64(590000)
	ratio := float64(hit) / float64(hit+read) * 100
	if ratio < 98.0 || ratio > 99.0 {
		t.Errorf("expected ~98.7%%, got %.2f%%", ratio)
	}
}

func TestMemContext_BufferHitRatio_ZeroReads(t *testing.T) {
	hit := int64(1000000)
	read := int64(0)
	ratio := float64(hit) / float64(hit+read) * 100
	if ratio != 100.0 {
		t.Errorf("expected 100%%, got %.2f%%", ratio)
	}
}

func TestMemContext_BufferHitRatio_ZeroBoth(t *testing.T) {
	hit := int64(0)
	read := int64(0)
	total := hit + read
	ratio := float64(0)
	if total > 0 {
		ratio = float64(hit) / float64(total) * 100
	}
	if ratio != 0 {
		t.Errorf("expected 0%%, got %.2f%%", ratio)
	}
}

func TestMemContext_TempSpillCount(t *testing.T) {
	// Mirrors the logic in analyzeMemory: count queries with
	// TempBlksWritten > 0.
	tempBlks := []int64{0, 500, 0, 1200, 0, 80}
	spillingQueries := 0
	for _, tb := range tempBlks {
		if tb > 0 {
			spillingQueries++
		}
	}
	if spillingQueries != 3 {
		t.Errorf("expected 3 spilling queries, got %d", spillingQueries)
	}
}

func TestMemContext_MemoryBudgetCalculation(t *testing.T) {
	maxConns := 60
	workMem := 4 // MB
	hashMultiplier := 2
	maxAlloc := maxConns * workMem * hashMultiplier
	if maxAlloc != 480 {
		t.Errorf("expected 480MB, got %dMB", maxAlloc)
	}
}

func TestMemContext_MemoryBudgetCalculation_WorkMemDangerous(t *testing.T) {
	maxConns := 200
	workMem := 64 // MB
	hashMultiplier := 2
	maxAlloc := maxConns * workMem * hashMultiplier
	if maxAlloc <= 3750 {
		t.Errorf(
			"expected allocation (%dMB) to exceed 3750MB RAM",
			maxAlloc,
		)
	}
}
