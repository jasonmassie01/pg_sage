package tuner

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// ErrHintMemory: an agent hint's work_mem is above the per-query budget,
// min(tuner.work_mem_max_mb, 5% of host memory) (tuning-03).
var ErrHintMemory = errors.New("hint work_mem above the memory budget")

// WithHostMemory gives the tuner live host memory (managed-cloud
// telemetry), which caps every work_mem hint at 5% of host RAM. Call it
// before the tuner is used; without it host memory is unknown and work_mem
// hints need an operator's approval.
func (t *Tuner) WithHostMemory(src advisor.HostMemorySource) {
	t.hostMemory = src
}

// WorkMemCeilingMB is the largest work_mem, in MB, a per-query hint may
// set: tuner.work_mem_max_mb (its default when unset), lowered to 5% of
// host memory when that is known.
func WorkMemCeilingMB(maxMB int, hostTotalBytes int64) (ceilingMB int, hostKnown bool) {
	ceilingMB = maxMB
	if ceilingMB <= 0 {
		ceilingMB = config.DefaultTunerWorkMemMaxMB
	}
	if hostTotalBytes <= 0 {
		return ceilingMB, false
	}
	if hostCap := advisor.MaxWorkMemBytes(hostTotalBytes) >> 20; hostCap < int64(ceilingMB) {
		ceilingMB = int(hostCap)
	}
	return ceilingMB, true
}

// workMemBudget is one reading of the per-query work_mem budget.
type workMemBudget struct {
	CeilingMB int
	MaxMB     int
	HostKnown bool
	HostMiB   int64
	HostCapMB int64
}

// workMemBudget reads host memory now (telemetry can change between cycles).
func (t *Tuner) workMemBudget() workMemBudget {
	var total int64
	if t.hostMemory != nil {
		total = t.hostMemory().TotalBytes
	}
	b := workMemBudget{MaxMB: t.cfg.WorkMemMaxMB}
	b.CeilingMB, b.HostKnown = WorkMemCeilingMB(t.cfg.WorkMemMaxMB, total)
	if b.HostKnown {
		b.HostMiB, b.HostCapMB = total>>20, advisor.MaxWorkMemBytes(total)>>20
	}
	return b
}

// detail records the budget arithmetic for a hint that asked for requestedMB.
func (b workMemBudget) detail(requestedMB, hintMB int) map[string]any {
	d := map[string]any{"rule": "min(tuner.work_mem_max_mb, 5% of host memory)",
		"requested_mb": requestedMB, "hint_mb": hintMB, "ceiling_mb": b.CeilingMB,
		"work_mem_max_mb": b.MaxMB, "host_memory_known": b.HostKnown,
		"clamped": hintMB < requestedMB}
	if b.HostKnown {
		d["host_memory_mib"], d["host_cap_mb"] = b.HostMiB, b.HostCapMB
	}
	return d
}

// capHint clamps a deterministic hint's work_mem to the ceiling; a ceiling
// under 1 MB drops the directive. The detail is nil without a work_mem.
func (b workMemBudget) capHint(hint string) (string, map[string]any) {
	mb, ok := extractWorkMemMB(hint)
	if !ok {
		return hint, nil
	}
	hintMB := min(mb, b.CeilingMB)
	if hintMB < 1 {
		hintMB = 0
	}
	out := make([]string, 0, 2)
	for _, d := range splitHintDirectives(hint) {
		d = strings.TrimSpace(d)
		if _, isWorkMem := extractWorkMemMB(d); isWorkMem {
			if hintMB == 0 {
				continue
			}
			d = fmtSetWorkMem(hintMB)
		}
		if d != "" {
			out = append(out, d)
		}
	}
	return strings.Join(out, " "), b.detail(mb, hintMB)
}

// refuse rejects an agent hint whose (already clamped) work_mem is above
// the ceiling, naming the bound that binds.
func (b workMemBudget) refuse(hint string) error {
	mb, ok := extractWorkMemMB(hint)
	switch {
	case !ok || mb <= b.CeilingMB:
		return nil
	case b.HostKnown && int64(mb) > b.HostCapMB:
		return fmt.Errorf("%w: work_mem %dMB is above 5%% of host memory (%d MB of %d MiB); "+
			"with hash_mem_multiplier, several plan nodes and concurrent executions it can "+
			"exhaust RAM", ErrHintMemory, mb, b.HostCapMB, b.HostMiB)
	default:
		return fmt.Errorf("%w: work_mem %dMB is above the %dMB ceiling "+
			"(tuner.work_mem_max_mb)", ErrHintMemory, mb, b.CeilingMB)
	}
}

// annotate records the budget on a finding whose hint sets work_mem and,
// with host memory unknown, marks it for an operator's approval.
func (b workMemBudget) annotate(f analyzer.Finding, hint string,
	detail map[string]any) analyzer.Finding {
	mb, ok := extractWorkMemMB(hint)
	if !ok {
		return f
	}
	if detail == nil {
		detail = b.detail(mb, mb)
	}
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail["work_mem_budget"] = detail
	if !b.HostKnown {
		f.Detail[analyzer.DetailApprovalRequired] = fmt.Sprintf("host memory is unknown, so "+
			"work_mem %dMB cannot be checked against 5%% of host RAM; an operator must "+
			"approve it", mb)
	}
	return f
}

// requestedWorkMemMB is the work_mem a hint asks for before the tuner's
// 64 MB floor and clamp; ok is false without a valid Set(work_mem).
func requestedWorkMemMB(hint string) (mb int, ok bool) {
	for _, d := range splitHintDirectives(hint) {
		m := setDirectiveRe.FindStringSubmatch(strings.TrimSpace(d))
		if m == nil || !strings.EqualFold(m[1], "work_mem") {
			continue
		}
		mb, err := parseWorkMemMB(strings.TrimSpace(m[2]))
		return mb, err == nil
	}
	return 0, false
}
