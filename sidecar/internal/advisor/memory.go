package advisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/workload"
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

	// Cache performance counts every statement; spills only workload
	// (maintenance spills in maintenance_work_mem, not work_mem).
	var totalBlksHit, totalBlksRead int64
	for _, q := range snap.Queries {
		totalBlksHit += q.SharedBlksHit
		totalBlksRead += q.SharedBlksRead
	}
	hitRatio := float64(0)
	if totalBlksHit+totalBlksRead > 0 {
		hitRatio = float64(totalBlksHit) /
			float64(totalBlksHit+totalBlksRead) * 100
	}
	spillingQueries, totalTempWritten, spills := spillSummary(snap.Queries)
	var spillLines []string
	for i, q := range spills {
		truncQuery := llm.SanitizeForLLM(q.query)
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

	// Ground the recommendation in the documented semantics and safe
	// ranges of the memory parameters (A3, GPTuner-style). The prior is
	// code-authored, so it travels in the system prompt, not the data.
	memoryDocs := DocContext(0,
		"shared_buffers", "work_mem", "maintenance_work_mem",
		"effective_cache_size")

	resp, err := chatAdvisor(ctx, mgr, "memory", memorySystemPrompt, memoryDocs, prompt)
	if err != nil {
		return nil, err
	}

	return parseLLMFindings(resp, "memory_tuning", logFn), nil
}

// spillQuery is a workload statement that spilled to temp files.
type spillQuery struct {
	query string
	temp  int64
	calls int64
}

// spillSummary counts the workload statements (internal/workload) that
// spilled, their temp blocks, and the five that spilled most.
func spillSummary(queries []collector.QueryStats) (int, int64, []spillQuery) {
	var spills []spillQuery
	var total int64
	for _, q := range workload.Queries(queries) {
		if q.TempBlksWritten > 0 {
			total += q.TempBlksWritten
			spills = append(spills, spillQuery{q.Query, q.TempBlksWritten, q.Calls})
		}
	}
	count := len(spills)
	sort.SliceStable(spills, func(i, j int) bool { return spills[i].temp > spills[j].temp })
	if len(spills) > 5 {
		spills = spills[:5]
	}
	return count, total, spills
}
