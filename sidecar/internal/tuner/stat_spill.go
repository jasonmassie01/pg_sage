package tuner

import (
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Stat-based temp spill sizing (tuning-03). pg_stat_statements counters
// are cumulative since the last reset, so a stat_temp_spill symptom is
// sized from the interval between two Tune cycles: the interval's spill
// per call, against the work_mem the statement's role runs with. The first
// sighting of a statement only records its counters; a fall in either
// counter (a stats reset) starts a new baseline.

// spillKey is one pg_stat_statements statement of one role.
type spillKey struct{ QueryID, UserID int64 }

func keyOf(c candidate) spillKey { return spillKey{c.QueryID, c.UserID} }

// spillBaseline is a statement's counters at the cycle it was last seen.
type spillBaseline struct {
	Calls, Blocks int64
	Seen          uint64
}

type spillDelta struct{ Calls, Blocks int64 }

// spillSettings are the server facts this cycle's sizing needs.
type spillSettings struct {
	BlockSize int64
	// WorkMemKB is each role's effective work_mem in kB, by userid.
	WorkMemKB map[int64]int64
}

// spillSettingsReader reads block_size and the work_mem of the roles.
type spillSettingsReader func(ctx context.Context, userIDs []int64) (spillSettings, error)

// statSpill is the tuner's stat-spill state; the tuner's mu guards it.
type statSpill struct {
	baselines map[spillKey]spillBaseline
	cycle     uint64
	needs     map[spillKey]spillSizing // this cycle's sized spills
	read      spillSettingsReader      // nil: readSpillSettings
}

// maxSpillBaselines bounds the baselines kept between cycles; above it
// the statement seen longest ago is forgotten (its next sighting is a
// first sighting again).
const maxSpillBaselines = 10_000

// advance records this cycle's counters and returns the statements whose
// calls and temp blocks both grew since their baseline.
func (s *statSpill) advance(cands []candidate) map[spillKey]spillDelta {
	if s.baselines == nil {
		s.baselines = map[spillKey]spillBaseline{}
	}
	s.cycle++
	rows := make(map[spillKey]int, len(cands))
	for _, c := range cands {
		rows[keyOf(c)]++
	}
	out := map[spillKey]spillDelta{}
	for _, c := range cands {
		k := keyOf(c)
		if rows[k] > 1 {
			// track = all splits a statement into top-level and nested
			// rows of one (queryid, userid): they cannot be diffed apart.
			delete(s.baselines, k)
			continue
		}
		prev, known := s.baselines[k]
		s.baselines[k] = spillBaseline{Calls: c.Calls, Blocks: c.TempBlksWritten, Seen: s.cycle}
		d := spillDelta{Calls: c.Calls - prev.Calls, Blocks: c.TempBlksWritten - prev.Blocks}
		if known && d.Calls > 0 && d.Blocks > 0 {
			out[k] = d
		}
	}
	s.evict()
	return out
}

// evict forgets the stalest baselines above maxSpillBaselines.
func (s *statSpill) evict() {
	for len(s.baselines) > maxSpillBaselines {
		var oldest spillKey
		first := true
		for k, b := range s.baselines {
			if first || b.Seen < s.baselines[oldest].Seen {
				oldest, first = k, false
			}
		}
		delete(s.baselines, oldest)
	}
}

// observeSpills sizes this cycle's stat spills from the interval deltas.
// Without the role's work_mem or the block size nothing is sized (fail
// closed). The caller holds t.mu.
func (t *Tuner) observeSpills(ctx context.Context, cands []candidate) {
	deltas := t.spill.advance(cands)
	t.spill.needs = nil
	if len(deltas) == 0 {
		return
	}
	read := t.spill.read
	if read == nil {
		read = t.readSpillSettings
	}
	settings, err := read(ctx, spillUsers(deltas, cands))
	if err != nil {
		t.logFn("WARN", "tuner: stat_temp_spill sizing skipped this cycle, the roles' "+
			"work_mem could not be read: %v", err)
		return
	}
	t.spill.needs = sizeSpills(deltas, settings, t.workMemBudget().CeilingMB)
	dropBelowPeers(t.spill.needs, cands, settings)
}

// spillUsers are the roles of the sized statements and every other role
// seen running one of them this cycle, sorted.
func spillUsers(deltas map[spillKey]spillDelta, cands []candidate) []int64 {
	users := make([]int64, 0, len(deltas))
	add := func(u int64) {
		if !slices.Contains(users, u) {
			users = append(users, u)
		}
	}
	for k := range deltas {
		add(k.UserID)
	}
	for _, c := range cands {
		for k := range deltas {
			if k.QueryID == c.QueryID {
				add(c.UserID)
				break
			}
		}
	}
	slices.Sort(users)
	return users
}

// dropBelowPeers drops a hint that would lower another role's work_mem: a
// pg_hint_plan hint applies to every role running the statement, so it may
// not be below the work_mem of another role seen running it this cycle,
// and a role whose work_mem is unknown blocks it (fail closed). Roles
// outside this cycle's candidates are not seen.
func dropBelowPeers(needs map[spillKey]spillSizing, cands []candidate, s spillSettings) {
	for k, sz := range needs {
		for _, c := range cands {
			if c.QueryID != k.QueryID || c.UserID == k.UserID {
				continue
			}
			if base, ok := s.WorkMemKB[c.UserID]; !ok || base > int64(sz.HintMB)*1024 {
				delete(needs, k)
				break
			}
		}
	}
}

// sizeSpills sizes each delta whose role work_mem and block size are known.
func sizeSpills(deltas map[spillKey]spillDelta, s spillSettings,
	ceilingMB int) map[spillKey]spillSizing {
	out := map[spillKey]spillSizing{}
	for k, d := range deltas {
		baseline, ok := s.WorkMemKB[k.UserID]
		if !ok || s.BlockSize <= 0 {
			continue
		}
		perCall := min(float64(d.Blocks)*float64(s.BlockSize)/float64(d.Calls),
			float64(maxSpillSizeKB)*1024)
		sz := spillSizing{DeltaCalls: d.Calls, DeltaBlocks: d.Blocks, BlockSize: s.BlockSize,
			PerCallKB: int64(math.Ceil(perCall / 1024)), BaselineKB: baseline,
			CeilingMB: ceilingMB}
		var hint bool
		sz.NeedMB, sz.HintMB, hint = sizeStatSpillWorkMem(sz.PerCallKB, baseline, ceilingMB)
		if hint {
			sz.Clamped = sz.HintMB < sz.NeedMB
			out[k] = sz
		}
	}
	return out
}

// spillSizing is the arithmetic of one stat-spill hint.
type spillSizing struct {
	DeltaCalls, DeltaBlocks, BlockSize int64
	PerCallKB, BaselineKB              int64
	NeedMB, CeilingMB, HintMB          int
	Clamped                            bool
}

func (s spillSizing) detail() map[string]any {
	return map[string]any{"delta_calls": s.DeltaCalls,
		"delta_temp_blks_written": s.DeltaBlocks, "block_size": s.BlockSize,
		"per_call_spill_kb": s.PerCallKB, "baseline_work_mem_kb": s.BaselineKB,
		"needed_mb": s.NeedMB, "ceiling_mb": s.CeilingMB, "hint_mb": s.HintMB,
		"clamped": s.Clamped}
}

// maxSpillSizeKB stops the doubling far above any real work_mem (1 PB).
const maxSpillSizeKB = int64(1) << 40

// sizeStatSpillWorkMem doubles the role's work_mem until it is at least
// twice the per-call spill (no floor), then clamps it to ceilingMB. There
// is no hint when the spill fits the baseline, or when the clamped size is
// not above the baseline (a no-op Set).
func sizeStatSpillWorkMem(perCallKB, baselineKB int64, ceilingMB int) (needMB, hintMB int,
	ok bool) {
	if baselineKB <= 0 || ceilingMB <= 0 || perCallKB <= baselineKB {
		return 0, 0, false
	}
	size := baselineKB
	for size < 2*perCallKB && size < maxSpillSizeKB {
		size *= 2
	}
	needMB = int((size + 1023) / 1024)
	hintMB = min(needMB, ceilingMB)
	if int64(hintMB)*1024 <= baselineKB {
		return needMB, 0, false
	}
	return needMB, hintMB, true
}

// statSpillRationale explains a stat-spill hint.
func statSpillRationale(d map[string]any) string {
	r := fmt.Sprintf("query spilled ~%v kB to disk per call over %v calls this interval "+
		"(role work_mem %v kB): work_mem %v MB", d["per_call_spill_kb"], d["delta_calls"],
		d["baseline_work_mem_kb"], d["hint_mb"])
	if d["clamped"] == true {
		r += fmt.Sprintf(" (clamped from %v MB to the %v MB ceiling)", d["needed_mb"],
			d["ceiling_mb"])
	}
	return r
}

// withSpillSizing records a stat-spill symptom's sizing on its finding.
func withSpillSizing(f analyzer.Finding, symptoms []PlanSymptom) analyzer.Finding {
	for _, s := range symptoms {
		if s.Kind == SymptomStatTempSpill && f.Detail != nil {
			f.Detail["stat_spill_sizing"] = s.Detail
		}
	}
	return f
}
