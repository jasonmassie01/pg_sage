package replay

import (
	"context"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Signal serves a recorded signal probe (probes.IsSignal: evidence
// produced in Go outside the SQL catalog, such as the pooler telemetry)
// the way Run serves a catalog probe: its next recorded observation, or
// unsupported not_recorded. It is the source of the investigation's
// sre.SignalProbe; the probe stays unknown to Run, so a model naming it
// is refused.
func (r *Runner) Signal(id probes.ID) func(context.Context, probes.Args) probes.Result {
	return func(ctx context.Context, args probes.Args) probes.Result {
		if r == nil {
			return probes.Result{ProbeID: id, Status: probes.StatusError,
				Reason: "not_configured"}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		res := probes.Result{ProbeID: id, ObservedAt: r.c.DetectedAt}
		if ctx.Err() != nil {
			res.Status, res.Reason = probes.StatusError, "canceled"
			r.calls = append(r.calls, Call{Probe: id, Args: args, Status: res.Status})
			return res
		}
		res, served := r.serve(id, res)
		r.calls = append(r.calls, Call{Probe: id, Args: args, Status: res.Status,
			Served: served})
		return res
	}
}

// Signals lists the signal probes the case records, in recorded order.
func (r *Runner) Signals() []probes.ID {
	var out []probes.ID
	seen := map[probes.ID]bool{}
	for _, o := range r.c.Observations {
		if probes.IsSignal(o.Probe) && !seen[o.Probe] {
			seen[o.Probe] = true
			out = append(out, o.Probe)
		}
	}
	return out
}
