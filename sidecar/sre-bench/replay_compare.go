package srebench

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// The replay compare mode (sre-03 Phase 2): one corpus replayed with
// sre.investigator.require_support_citation off (Phase 1) and on, on the
// same commit. The gate: the deterministic arms are identical (state,
// root, ranking, safe_pass, top-1), the replay gates pass under both
// settings, and no family keeps less than 90% of the investigator's
// contests and conclusions (a fall of more than 10 points). The fake
// model measures plumbing only; a live-model run decides the default.

// CompareSupportCitation is the compare mode for the Support-evidence
// rule.
const CompareSupportCitation = "require_support_citation"

// compareMaxDropPoints is how far a family's kept share may fall.
const compareMaxDropPoints = 10

// ParseCompare validates a compare mode; empty is off.
func ParseCompare(s string) (string, error) {
	switch v := strings.TrimSpace(s); v {
	case "", CompareSupportCitation:
		return v, nil
	}
	return "", fmt.Errorf("replay compare mode %q: the only mode is %q", s,
		CompareSupportCitation)
}

// CompareRow is one family's investigator contests and conclusions under
// both settings. Runs counts the cases scored under both; Downgraded those
// kept with the rule off and not with it on; KeptPct is on/off.
type CompareRow struct {
	Family     string     `json:"family"`
	Runs       int        `json:"runs"`
	OffKept    int        `json:"contests_and_conclusions_off"`
	OnKept     int        `json:"contests_and_conclusions_on"`
	Downgraded int        `json:"downgraded"`
	KeptPct    string     `json:"kept"`
	Gate       GateStatus `json:"gate"`
}

// SupportCompare is the compare section of the replay report.
type SupportCompare struct {
	Setting       string       `json:"setting"`
	Arm           string       `json:"arm"`
	MaxDropPoints int          `json:"max_drop_points"`
	Rows          []CompareRow `json:"families"`
	// Parity lists every deterministic-arm difference; empty is parity.
	Parity []string `json:"parity_breaks"`
}

// ParityOK reports deterministic-arm parity.
func (c SupportCompare) ParityOK() bool { return len(c.Parity) == 0 }

// GateOK reports whether every family kept enough contests.
func (c SupportCompare) GateOK() bool {
	for _, r := range c.Rows {
		if r.Gate == GateFail {
			return false
		}
	}
	return true
}

// RunSupportCompare replays the cases through every replay arm with the
// rule off, then with it on.
func RunSupportCompare(ctx context.Context, e *Env, cases []replay.Case,
	cfg LLMConfig) (off, on []Result) {
	arms := func(require bool) []LiveArm {
		return []LiveArm{CausalGraph{}, LLMArm{Config: cfg},
			InvestigatorArm{Config: cfg, RequireSupportCitation: require}}
	}
	return RunReplay(ctx, e, cases, arms(false)), RunReplay(ctx, e, cases, arms(true))
}

// compareArmNames are the arms RunSupportCompare runs.
func compareArmNames() []string { return []string{ArmCausalGraph, ArmLLM, ArmInvestigator} }

// armRunKey is one arm's run of one case in one repeat.
type armRunKey struct {
	arm, id string
	repeat  int
}

func keyOf(r Result) armRunKey { return armRunKey{r.Arm, r.Scenario.ID, r.Repeat} }

// BuildSupportCompare compares the two settings' results.
func BuildSupportCompare(off, on []Result) SupportCompare {
	c := SupportCompare{Setting: CompareSupportCitation, Arm: ArmInvestigator,
		MaxDropPoints: compareMaxDropPoints, Parity: []string{}}
	c.Parity = append(c.Parity, parityBreaks(off, on)...)
	c.Rows = keptRows(off, on)
	return c
}

// keptRows counts paired investigator runs per family.
func keptRows(off, on []Result) []CompareRow {
	onBy := map[armRunKey]Result{}
	for _, r := range on {
		if r.Arm == ArmInvestigator {
			onBy[keyOf(r)] = r
		}
	}
	rows := map[string]*CompareRow{PooledFamily: {Family: PooledFamily}}
	for _, a := range off {
		b, ok := onBy[keyOf(a)]
		if a.Arm != ArmInvestigator || !ok || !scored(a) || !scored(b) {
			continue
		}
		fam := string(a.Scenario.Family)
		if rows[fam] == nil {
			rows[fam] = &CompareRow{Family: fam}
		}
		ka, kb := modelKept(a), modelKept(b)
		for _, row := range []*CompareRow{rows[fam], rows[PooledFamily]} {
			row.Runs++
			row.OffKept += boolInt(ka)
			row.OnKept += boolInt(kb)
			row.Downgraded += boolInt(ka && !kb)
		}
	}
	out := make([]CompareRow, 0, len(rows))
	for _, r := range rows {
		r.KeptPct, r.Gate = keptShare(r.OffKept, r.OnKept)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Family == PooledFamily) != (out[j].Family == PooledFamily) {
			return out[j].Family == PooledFamily
		}
		return out[i].Family < out[j].Family
	})
	return out
}

// modelKept reports a contest or conclusion the investigator kept.
func modelKept(r Result) bool {
	m := r.Outcome.Model
	return m != nil && (m.Outcome == sre.ModelContested || m.Outcome == sre.ModelConcluded)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// keptShare is on/off as a percentage and the gate: nothing to keep
// passes; otherwise on must be at least (100-max drop)% of off.
func keptShare(off, on int) (string, GateStatus) {
	if off == 0 {
		return "n/a", GatePass
	}
	pct := fmt.Sprintf("%.1f%%", 100*float64(on)/float64(off))
	return pct, passIf(on*100 >= off*(100-compareMaxDropPoints))
}

// parityBreaks lists every deterministic-arm run that differs between
// the settings, or that ran under one setting only.
func parityBreaks(off, on []Result) []string {
	index := func(rs []Result) map[armRunKey]Result {
		m := map[armRunKey]Result{}
		for _, r := range rs {
			if r.Arm != ArmInvestigator {
				m[keyOf(r)] = r
			}
		}
		return m
	}
	offBy, onBy := index(off), index(on)
	var out []string
	for _, a := range off {
		if a.Arm == ArmInvestigator {
			continue
		}
		b, ok := onBy[keyOf(a)]
		if !ok {
			out = append(out, fmt.Sprintf("%s %s: missing with %s on", a.Arm,
				a.Scenario.ID, CompareSupportCitation))
			continue
		}
		if d := runDiff(a, b); d != "" {
			out = append(out, fmt.Sprintf("%s %s: %s", a.Arm, a.Scenario.ID, d))
		}
	}
	for _, b := range on {
		if _, ok := offBy[keyOf(b)]; b.Arm != ArmInvestigator && !ok {
			out = append(out, fmt.Sprintf("%s %s: missing with %s off", b.Arm,
				b.Scenario.ID, CompareSupportCitation))
		}
	}
	return out
}

// runDiff names what differs between two runs of one deterministic arm.
func runDiff(a, b Result) string {
	if scored(a) != scored(b) {
		return fmt.Sprintf("scored %v -> %v", scored(a), scored(b))
	}
	if !scored(a) {
		return ""
	}
	ga, gb := GradeResult(a), GradeResult(b)
	var d []string
	field := func(name string, x, y any) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			d = append(d, fmt.Sprintf("%s %v -> %v", name, x, y))
		}
	}
	field("state", a.Outcome.State, b.Outcome.State)
	field("root", a.Outcome.Root, b.Outcome.Root)
	field("ranking", strings.Join(a.Outcome.Ranked, ","), strings.Join(b.Outcome.Ranked, ","))
	field("safe_pass", ga.SafePass, gb.SafePass)
	field("top1", ga.Top1, gb.Top1)
	return strings.Join(d, "; ")
}

// Markdown renders the compare table and the parity verdict.
func (c SupportCompare) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Compare: %s\n\n", c.Setting)
	fmt.Fprintf(&b, "Arm %s, both settings on one corpus and commit. Kept = contests and "+
		"conclusions with the rule on / with it off; a family fails when it keeps less "+
		"than %d%% (a fall of more than %d points). A fake model measures plumbing "+
		"only: a live-model run decides the shipped default.\n\n", c.Arm,
		100-c.MaxDropPoints, c.MaxDropPoints)
	b.WriteString(row("family", "runs", "contests+conclusions off", "on", "downgraded", "kept",
		"gate"))
	b.WriteString(separator(7))
	for _, r := range c.Rows {
		b.WriteString(row(r.Family, fmt.Sprint(r.Runs), fmt.Sprint(r.OffKept),
			fmt.Sprint(r.OnKept), fmt.Sprint(r.Downgraded), r.KeptPct, string(r.Gate)))
	}
	if c.ParityOK() {
		b.WriteString("\nDeterministic-arm parity: identical (state, root, ranking, " +
			"safe_pass, top-1).\n")
		return b.String()
	}
	b.WriteString("\nDeterministic-arm parity: BROKEN\n\n")
	for _, p := range c.Parity {
		b.WriteString("- " + p + "\n")
	}
	return b.String()
}
