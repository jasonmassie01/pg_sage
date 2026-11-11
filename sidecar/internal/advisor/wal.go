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
1. Judge checkpoint pressure by the requested share of the window's
   checkpoints: above 20% means max_wal_size is too small for the WAL the
   workload writes between timed checkpoints.
2. Size max_wal_size from the measured WAL per checkpoint_timeout. Show the
   math with the facts given; never invent a fact stated as unknown.
3. NEVER recommend max_wal_size below its current value or below "Lowest
   max_wal_size", nor above "Highest max_wal_size". If the lowest is
   unknown, do not recommend a max_wal_size value.
4. For managed services, only recommend adjustable settings.
5. If checkpoints keep up (requested share under 20%), return [].
6. All recommendations are ALTER SYSTEM SET.
7. Flag if pending_restart = true.

Each element: {"object_identifier":"instance","severity":"info",` +
	`"rationale":"...","recommended_sql":"ALTER SYSTEM SET ...",` +
	`"current_settings":{...},"recommended_settings":{...},` +
	`"requires_restart":false}`

// walGUCs are the documented GUCs the WAL advisor tunes.
var walGUCs = []string{"max_wal_size", "min_wal_size", "checkpoint_timeout",
	"checkpoint_completion_target", "wal_buffers"}

// walSettingNames are the settings the WAL prompt shows.
var walSettingNames = map[string]bool{"max_wal_size": true, "min_wal_size": true,
	"checkpoint_completion_target": true, "wal_compression": true, "wal_level": true,
	"wal_buffers": true, "checkpoint_timeout": true, "full_page_writes": true}

// analyzeWAL reviews checkpoints and WAL over the advisor window
// (advisors-05). Without the window's history every finding is advice; a
// history read error skips the review; a requested share under 20% needs
// no model call. The model narrates and may raise max_wal_size between the
// measured floor and 4 x the floor.
func analyzeWAL(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	if snap.ConfigData == nil {
		return nil, nil
	}
	w := advisorWindowFrom(ctx, cfg.Advisor.Interval())
	facts, err := walFacts(ctx, w.src, snap, w.window)
	if err != nil {
		return nil, fmt.Errorf("wal: skipped, nothing is recommended from one snapshot: %w",
			err)
	}
	if r := facts.requestedRatio; r != nil && *r < walSkipRatio {
		logIf(logFn, "DEBUG", "advisor: wal: %.1f%% of the window's checkpoints were "+
			"requested (under %.0f%%): checkpoints keep up, no model call", *r*100,
			walSkipRatio*100)
		return nil, nil
	}
	fl := walSizeFloor(facts)
	gate := openGateFrom(ctx)
	grounding := promptGrounding(ctx, walGUCs...)
	resp, err := chatAdvisor(ctx, mgr, "wal", walSystemPrompt, grounding,
		buildWALPrompt(snap, gate, facts, fl))
	if err != nil {
		return nil, err
	}
	findings := filterUnsafeMaxWALSize(parseLLMFindings(resp, "wal_tuning", logFn),
		currentMaxWALSize(snap.ConfigData.PGSettings), fl, facts, logFn)
	if !facts.known {
		for i, f := range findings {
			if f.RecommendedSQL != "" {
				findings[i] = advisoryOnly(f, "the advisor window's checkpoint history "+
					"is unknown ("+facts.unknownWhy+")")
			}
		}
	}
	return findings, nil
}

// buildWALPrompt states the settings (those with an open finding left out,
// advisors-02) and the window's facts.
func buildWALPrompt(snap *collector.Snapshot, gate openGate, facts walWindowFacts,
	fl walFloor) string {
	var settings []string
	for _, s := range snap.ConfigData.PGSettings {
		if !walSettingNames[s.Name] || gate.hasGUC("wal_tuning", s.Name) {
			continue
		}
		restart := ""
		if s.PendingRestart {
			restart = " (pending_restart=true)"
		}
		settings = append(settings, fmt.Sprintf("  %s = %s%s", s.Name, settingText(s), restart))
	}
	unloggedNote := ""
	if n := countUnloggedTables(snap); n > 0 {
		unloggedNote = fmt.Sprintf("\n\nNote: %d unlogged table(s) detected. "+
			"Unlogged tables do NOT generate WAL, so low WAL activity may be "+
			"expected if writes target them.", n)
	}
	return fmt.Sprintf("WAL & CHECKPOINT CONTEXT:\n\nCurrent settings:\n%s\n\n%s\n\n"+
		"Observed metrics:\n  Total checkpoints since the statistics epoch: %d\n"+
		"  WAL position: %s%s%s",
		strings.Join(settings, "\n"), walFactLines(facts, fl),
		snap.System.TotalCheckpoints, snap.ConfigData.WALPosition, unloggedNote,
		gucNote(gate, "wal_tuning"))
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
