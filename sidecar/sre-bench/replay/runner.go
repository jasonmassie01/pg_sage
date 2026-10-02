package replay

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// ReasonNotRecorded is the reason of a probe the case did not record (or
// asked for more often than recorded): the replay has no evidence from
// after detection, so it answers "unsupported", never a healthy empty.
const ReasonNotRecorded = "not_recorded"

// Call is one probe call the runner received.
type Call struct {
	Probe  probes.ID
	Args   probes.Args
	Status probes.Status
	Served bool
}

// Runner replays one case: each catalog probe call gets that probe's
// next recorded observation, observed at detection plus its offset. It
// implements sre.ProbeRunner. Calls outside the catalog, or with
// arguments the probe does not take, are refused as probes.Runner
// refuses them and reported by Forbidden.
type Runner struct {
	c   Case
	reg *probes.Registry

	mu        sync.Mutex
	next      map[probes.ID]int
	calls     []Call
	forbidden []string
}

// NewRunner replays c against the probe catalog reg.
func NewRunner(c Case, reg *probes.Registry) *Runner {
	return &Runner{c: c, reg: reg, next: map[probes.ID]int{}}
}

// Run serves the probe's next recorded observation.
func (r *Runner) Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result {
	if r == nil || r.reg == nil {
		return probes.Result{ProbeID: id, Status: probes.StatusError,
			Reason: "not_configured"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	res := probes.Result{ProbeID: id, ObservedAt: r.c.DetectedAt}
	spec, ok := r.reg.Spec(id)
	switch {
	case !ok:
		res.Status, res.Reason = probes.StatusError, "unknown_probe"
	case r.reg.CheckArgs(id, args) != nil:
		res.Status, res.Reason = probes.StatusError, "invalid_args"
	case ctx.Err() != nil:
		res.Status, res.Reason = probes.StatusError, "canceled"
	}
	if res.Status != "" {
		if res.Reason != "canceled" {
			r.forbidden = append(r.forbidden, "tool:"+string(id)+": "+res.Reason)
		}
		r.calls = append(r.calls, Call{Probe: id, Args: args, Status: res.Status})
		return res
	}
	res.Version = spec.Version
	res, served := r.serve(id, res)
	r.calls = append(r.calls, Call{Probe: id, Args: args, Status: res.Status,
		Served: served})
	return res
}

// serve fills res from the probe's next recorded observation.
func (r *Runner) serve(id probes.ID, res probes.Result) (probes.Result, bool) {
	k := 0
	for _, o := range r.c.Observations {
		if o.Probe != id {
			continue
		}
		if k == r.next[id] {
			r.next[id]++
			res.Status, res.Reason, res.Error = o.Status, o.Reason, o.Error
			res.ObservedAt, res.Truncated = r.c.At(o), o.Truncated
			res.Rows = copyRows(o.Rows)
			res.Columns = columns(o.Rows)
			return res, true
		}
		k++
	}
	res.Status, res.Reason = probes.StatusUnsupported, ReasonNotRecorded
	return res, false
}

// copyRows deep-copies rows through JSON, so callers cannot change the
// frozen case. Numbers stay json.Number.
func copyRows(rows []probes.Row) []probes.Row {
	if len(rows) == 0 {
		return nil
	}
	out := make([]probes.Row, 0, len(rows))
	for _, row := range rows {
		cp := make(probes.Row, len(row))
		for k, v := range row {
			cp[k] = copyValue(v)
		}
		out = append(out, cp)
	}
	return out
}

func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any, []any:
		raw, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		var cp any
		if json.Unmarshal(raw, &cp) != nil {
			return nil
		}
		return cp
	default:
		return v // strings, numbers, booleans and nil are immutable
	}
}

func columns(rows []probes.Row) []string {
	set := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Calls lists every call the runner received, in order.
func (r *Runner) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Forbidden lists the refused calls: a probe outside the catalog or
// arguments the probe does not take.
func (r *Runner) Forbidden() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.forbidden...)
}
