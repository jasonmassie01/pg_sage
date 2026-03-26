package advisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

const memorySystemPrompt = `You are a PostgreSQL memory tuning expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. shared_buffers: typically 25% of RAM. If cache hit ratio > 99%, ` +
	`well-sized. < 95% recommend increase.
2. work_mem: calculate max possible = max_connections * work_mem * ` +
	`hash_mem_multiplier. Must fit in RAM.
3. If sort/hash spills > 100/day, work_mem too low.
4. Show the math: spill volume, proposed work_mem, memory impact.
5. effective_cache_size ~ 75% of total RAM.
6. Never recommend work_mem > 256MB without OOM warning.
7. maintenance_work_mem: 256MB-1GB based on largest table.
8. If everything is healthy (hit ratio > 99%, spills < 50/day), return [].

Each element: {"object_identifier":"instance","severity":"info",` +
	`"rationale":"...","recommended_sql":"ALTER SYSTEM SET ...",` +
	`"current_settings":{...},"recommended_settings":{...}}`

func analyzeMemory(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	if snap.ConfigData == nil {
		return nil, nil
	}

	// Memory settings.
	var memSettings []string
	for _, s := range snap.ConfigData.PGSettings {
		switch s.Name {
		case "shared_buffers", "work_mem", "maintenance_work_mem",
			"effective_cache_size", "huge_pages", "temp_buffers",
			"hash_mem_multiplier", "max_connections":
			memSettings = append(memSettings,
				fmt.Sprintf("  %s = %s%s",
					s.Name, s.Setting, s.Unit),
			)
		}
	}

	// Cache performance from queries.
	var totalBlksHit, totalBlksRead, totalTempWritten int64
	var spillingQueries int
	for _, q := range snap.Queries {
		totalBlksHit += q.SharedBlksHit
		totalBlksRead += q.SharedBlksRead
		totalTempWritten += q.TempBlksWritten
		if q.TempBlksWritten > 0 {
			spillingQueries++
		}
	}

	hitRatio := float64(0)
	if totalBlksHit+totalBlksRead > 0 {
		hitRatio = float64(totalBlksHit) /
			float64(totalBlksHit+totalBlksRead) * 100
	}

	// Top spilling queries.
	type spillQuery struct {
		query string
		temp  int64
		calls int64
	}
	var spills []spillQuery
	for _, q := range snap.Queries {
		if q.TempBlksWritten > 0 {
			spills = append(spills, spillQuery{
				q.Query, q.TempBlksWritten, q.Calls,
			})
		}
	}

	// Sort by temp blocks desc (simple selection sort, max 5).
	var spillLines []string
	for i := 0; i < len(spills) && i < 5; i++ {
		maxIdx := i
		for j := i + 1; j < len(spills); j++ {
			if spills[j].temp > spills[maxIdx].temp {
				maxIdx = j
			}
		}
		spills[i], spills[maxIdx] = spills[maxIdx], spills[i]
		q := spills[i]
		truncQuery := q.query
		if len(truncQuery) > 120 {
			truncQuery = truncQuery[:120] + "..."
		}
		spillLines = append(spillLines, fmt.Sprintf(
			"  Q%d: %s\n      calls=%d, temp_blks_written=%d",
			i+1, truncQuery, q.calls, q.temp,
		))
	}

	platform := detectPlatform(snap.ConfigData.PGSettings)

	prompt := fmt.Sprintf(
		"MEMORY CONTEXT:\n\n"+
			"Settings:\n%s\n\n"+
			"Cache performance:\n"+
			"  Buffer cache hit ratio: %.1f%%\n"+
			"  Shared blocks hit: %d\n"+
			"  Shared blocks read: %d\n\n"+
			"Sort/Hash spills:\n"+
			"  Queries with temp writes: %d\n"+
			"  Total temp blocks written: %d\n"+
			"  Top spilling queries:\n%s\n\n"+
			"Platform: %s",
		strings.Join(memSettings, "\n"),
		hitRatio,
		totalBlksHit, totalBlksRead,
		spillingQueries, totalTempWritten,
		strings.Join(spillLines, "\n"),
		platform,
	)

	if len(prompt) > maxAdvisorPromptChars {
		prompt = prompt[:maxAdvisorPromptChars]
	}

	resp, _, err := mgr.ChatForPurpose(
		ctx, "advisor", memorySystemPrompt, prompt, 4096,
	)
	if err != nil {
		return nil, fmt.Errorf("memory LLM: %w", err)
	}

	return parseLLMFindings(resp, "memory_tuning", logFn), nil
}
