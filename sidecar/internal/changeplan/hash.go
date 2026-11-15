package changeplan

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// HashPrefix versions the plan hash's canonical encoding. A new hashed
// field joins the hash only with a new prefix version.
const HashPrefix = "plan-v1"

// PlanHash is the SHA-256 that binds an approval to a plan: over the
// prefix, the goal, scope, origin and pack hash, then each step in
// ordinal order (every field except the runtime ones, with defaults
// applied) and the sorted edges. Each part ends in a NUL and every list
// is counted, so no bytes move between fields. Sets (scope, evidence ids,
// targets, guardrails, edges) are sorted: their order is not content.
func PlanHash(p Plan) string {
	h := &hasher{sum: sha256.New()}
	h.str(HashPrefix)
	h.str(p.Goal)
	h.strs(p.Scope.Objects)
	h.ints(p.Scope.QueryIDs)
	h.str(string(p.Origin))
	h.str(p.PackHash)
	steps := slices.Clone(p.Steps)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Ordinal < steps[j].Ordinal })
	h.num(int64(len(steps)))
	for _, s := range steps {
		h.step(s.WithDefaults())
	}
	edges := slices.Clone(p.Edges)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	h.num(int64(len(edges)))
	for _, e := range edges {
		h.num(int64(e.From))
		h.num(int64(e.To))
	}
	return hex.EncodeToString(h.sum.Sum(nil))
}

func (h *hasher) step(s Step) {
	h.num(int64(s.Ordinal))
	for _, part := range []string{string(s.Kind), s.ActionType, s.Setting, s.ProposedSQL,
		s.RollbackSQL} {
		h.str(part)
	}
	h.optInt(s.FindingID)
	h.strs(s.EvidenceIDs)
	h.ints(s.TargetQueryIDs)
	h.str(string(s.Metric))
	h.str(string(s.ExpectedDirection))
	h.optFloat(s.ExpectedPct)
	h.str(string(s.ExpectedSource))
	guards := slices.Clone(s.Guardrails)
	sort.Slice(guards, func(i, j int) bool {
		if guards[i].Metric != guards[j].Metric {
			return guards[i].Metric < guards[j].Metric
		}
		return guards[i].MaxRegressPct < guards[j].MaxRegressPct
	})
	h.num(int64(len(guards)))
	for _, g := range guards {
		h.str(string(g.Metric))
		h.float(g.MaxRegressPct)
	}
	h.num(s.MinEvidence.MinCalls)
	h.num(int64(s.MinEvidence.MinSamples))
	h.num(int64(s.DeadlineMinutes))
	h.str(strconv.FormatBool(s.Checkpoint))
	for _, a := range []OutcomeAction{s.OnImproved, s.OnNoChange, s.OnRegressed,
		s.OnInconclusive, s.OnUnverifiable, s.OnExpired} {
		h.str(string(a))
	}
}

type hasher struct{ sum hash.Hash }

func (h *hasher) str(s string) {
	// A NUL inside a value is escaped so it cannot end the field.
	h.sum.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\x00", `\0`)))
	h.sum.Write([]byte{0})
}

func (h *hasher) num(n int64) { h.str(strconv.FormatInt(n, 10)) }

func (h *hasher) float(f float64) { h.str(strconv.FormatFloat(f, 'g', -1, 64)) }

func (h *hasher) strs(list []string) {
	sorted := slices.Clone(list)
	sort.Strings(sorted)
	h.num(int64(len(sorted)))
	for _, s := range sorted {
		h.str(s)
	}
}

func (h *hasher) ints(list []int64) {
	sorted := slices.Clone(list)
	slices.Sort(sorted)
	h.num(int64(len(sorted)))
	for _, n := range sorted {
		h.num(n)
	}
}

func (h *hasher) optInt(n *int64) {
	if n == nil {
		h.str("null")
		return
	}
	h.num(*n)
}

func (h *hasher) optFloat(f *float64) {
	if f == nil {
		h.str("null")
		return
	}
	h.float(*f)
}
