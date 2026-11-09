package advisor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const walSystemPrompt = `You are a PostgreSQL WAL and checkpoint tuning expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. If checkpoints_req > 20% of total, max_wal_size is too small.
2. If buffers_backend > 5% of total buffers written, checkpoints can't keep up.
3. Recommend specific values for max_wal_size based on WAL generation rate.
4. For managed services, only recommend adjustable settings.
5. Show the math: current WAL between checkpoints, recommended max_wal_size.
6. If everything is healthy (< 5% forced checkpoints), return [].
7. All recommendations are ALTER SYSTEM SET.
8. Flag if pending_restart = true.
9. Never recommend reducing max_wal_size below current.

Each element: {"object_identifier":"instance","severity":"info",` +
	`"rationale":"...","recommended_sql":"ALTER SYSTEM SET ...",` +
	`"current_settings":{...},"recommended_settings":{...},` +
	`"requires_restart":false}`

// walGUCs are the documented GUCs the WAL advisor tunes.
var walGUCs = []string{"max_wal_size", "min_wal_size", "checkpoint_timeout",
	"checkpoint_completion_target", "wal_buffers"}

func analyzeWAL(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	prev *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	if snap.ConfigData == nil {
		return nil, nil
	}

	// Build WAL settings context; settings with an open finding are left
	// out (advisors-02).
	gate := openGateFrom(ctx)
	var walSettings []string
	for _, s := range snap.ConfigData.PGSettings {
		if gate.hasGUC("wal_tuning", s.Name) {
			continue
		}
		switch s.Name {
		case "max_wal_size", "min_wal_size",
			"checkpoint_completion_target",
			"wal_compression", "wal_level", "wal_buffers",
			"checkpoint_timeout", "full_page_writes":
			restart := ""
			if s.PendingRestart {
				restart = " (pending_restart=true)"
			}
			walSettings = append(walSettings,
				fmt.Sprintf("  %s = %s%s", s.Name, settingText(s), restart))
		}
	}

	// Checkpoint metrics from system stats.
	sys := snap.System
	totalCP := sys.TotalCheckpoints
	var prevCP int64
	if prev != nil {
		prevCP = prev.System.TotalCheckpoints
	}
	cpDelta := totalCP - prevCP

	// Count unlogged tables so the LLM knows some tables don't generate WAL.
	unloggedNote := ""
	unloggedCount := countUnloggedTables(snap)
	if unloggedCount > 0 {
		unloggedNote = fmt.Sprintf(
			"\n\nNote: %d unlogged table(s) detected. "+
				"Unlogged tables do NOT generate WAL, so low WAL "+
				"activity may be expected if writes target them.",
			unloggedCount,
		)
	}

	prompt := fmt.Sprintf(
		"WAL & CHECKPOINT CONTEXT:\n\n"+
			"Current settings:\n%s\n\n"+
			"Observed metrics:\n"+
			"  Total checkpoints: %d (delta since last: %d)\n"+
			"  WAL position: %s%s%s",
		strings.Join(walSettings, "\n"),
		totalCP, cpDelta,
		snap.ConfigData.WALPosition,
		unloggedNote, gucNote(gate, "wal_tuning"),
	)

	grounding := promptGrounding(ctx, walGUCs...)
	resp, err := chatAdvisor(ctx, mgr, "wal", walSystemPrompt, grounding, prompt)
	if err != nil {
		return nil, err
	}

	return parseLLMFindings(resp, "wal_tuning", logFn), nil
}

// settingText renders a pg_settings value as SHOW does for a memory unit
// that counts pages (wal_buffers in "8kB" is "4MB", not "5128kB"); other
// values keep their unit suffix. A negative page count is a sentinel
// (-1: auto) and stays bare.
func settingText(s collector.PGSetting) string {
	if s.Unit == "" || !unicode.IsDigit(rune(s.Unit[0])) {
		return s.Setting + s.Unit
	}
	n, err := strconv.ParseFloat(s.Setting, 64)
	if err != nil {
		return s.Setting + s.Unit
	}
	if n < 0 {
		return s.Setting
	}
	bytes, err := pgconf.ParseValue(s.Setting, pgconf.GUCDoc{Unit: "bytes", BaseUnit: s.Unit})
	if err != nil {
		return s.Setting + s.Unit
	}
	for _, u := range []struct {
		name string
		size float64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}} {
		if bytes >= u.size && math.Mod(bytes, u.size) == 0 {
			return strconv.FormatFloat(bytes/u.size, 'f', -1, 64) + u.name
		}
	}
	return strconv.FormatFloat(bytes/(1<<10), 'f', -1, 64) + "kB"
}

// countUnloggedTables returns the number of tables with relpersistence='u'.
func countUnloggedTables(snap *collector.Snapshot) int {
	count := 0
	for _, t := range snap.Tables {
		if t.IsUnlogged() {
			count++
		}
	}
	return count
}
