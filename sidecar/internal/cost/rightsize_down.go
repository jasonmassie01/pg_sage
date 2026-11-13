package cost

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

const gib = float64(1 << 30)

// downsize steps one size down only with headroom: projected CPU p95 at
// most 60% and peak at most 85%, and the smaller size's RAM holds
// shared_buffers plus the used-memory p95. Never onto a burstable class.
func downsize(v Verdict, st usageStats, in RightsizeInput) Verdict {
	target := nextSmaller(v.CurrentClass)
	if target == "" {
		return v.keep("no smaller size in the family is known")
	}
	if cloudtel.IsBurstableClass(target) {
		return v.keep(fmt.Sprintf("never downsizes onto a burstable class (%s)", target))
	}
	tv := classVCPUs(target)
	if tv <= 0 {
		return v.keep("the smaller size has no documented vCPU count")
	}
	scale := float64(v.CurrentVCPUs) / float64(tv)
	p95, peak := v.CPUP95*scale, v.CPUMax*scale
	if p95 > downMaxP95Pct || peak > downMaxPeakPct {
		return v.keep(fmt.Sprintf("projected CPU at %s: p95 %.1f%%, max %.1f%% "+
			"(limits %.0f%% and %.0f%%)", target, p95, peak, downMaxP95Pct, downMaxPeakPct))
	}
	mem := classMemoryBytes(target)
	if st.usedP95 == nil || mem <= 0 {
		return v.keep("memory use unknown: the smaller size cannot be shown to fit")
	}
	if need := in.SharedBuffersBytes + *st.usedP95; mem < need {
		return v.keep(fmt.Sprintf("memory: %s has %.1f GiB; shared_buffers plus used "+
			"memory p95 need %.1f GiB", target, mem/gib, need/gib))
	}
	reason := fmt.Sprintf("CPU p95 %.1f%% and max %.1f%% project to %.1f%% and %.1f%% "+
		"on %s, and its RAM fits", v.CPUP95, v.CPUMax, p95, peak, target)
	return v.toward(target, DecisionDownsize, reason, in.Rates)
}

func (v Verdict) keep(reason string) Verdict {
	v.Decision, v.Reason = DecisionKeep, reason
	return v
}

// toward records a move to target with its projection and, with rates,
// the monthly compute delta, linear in vCPUs from the configured price.
func (v Verdict) toward(target, decision, reason string, rates *Rates) Verdict {
	tv := classVCPUs(target)
	if target == "" || tv <= 0 {
		return v.keep(reason + "; no " + decision + " size in the family is known")
	}
	v.Decision, v.Reason, v.TargetClass, v.TargetVCPUs = decision, reason, target, tv
	scale := float64(v.CurrentVCPUs) / float64(tv)
	v.ProjectedCPUP95, v.ProjectedCPUMax = v.CPUP95*scale, v.CPUMax*scale
	if rates != nil {
		delta := rates.ComputePerHour * (float64(tv)/float64(v.CurrentVCPUs) - 1) *
			hoursPerMonth
		v.MonthlyDelta, v.Currency = &delta, rates.Currency
		v.RateSource, v.AsOf = rates.Source, rates.AsOf
	}
	return v
}
