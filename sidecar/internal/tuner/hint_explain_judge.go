package tuner

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/workload"
)

// maxEvidencePlanBytes caps each plan kept in hint_evidence for the
// approval card; a larger plan is left out and named.
const maxEvidencePlanBytes = 64 << 10

// explainableKeywords are the statements EXPLAIN plans without running.
var explainableKeywords = map[string]bool{
	"SELECT": true, "WITH": true, "VALUES": true, "TABLE": true,
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true,
}

var (
	bindParamRe    = regexp.MustCompile(`\$[0-9]+`)
	firstKeywordRe = regexp.MustCompile(`^\s*\(*\s*([A-Za-z]+)`)
)

// explainableBody returns stmt without a trailing semicolon, or why it may
// not be planned: not one explainable statement, or a hint that could leave
// its comment.
func explainableBody(stmt, hint string) (string, string) {
	if !validateHintSyntax(hint) || strings.Contains(hint, "/*") ||
		strings.Contains(hint, "*/") {
		return "", fmt.Sprintf("%q is not one pg_hint_plan comment body", hint)
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
	if body == "" {
		return "", "empty statement"
	}
	// Any semicolon left, even in a literal, refuses: the plans run over the
	// simple protocol, which would run a second statement.
	if strings.Contains(body, ";") {
		return "", "multi-statement text is never planned"
	}
	m := firstKeywordRe.FindStringSubmatch(workload.BlankLiteralsAndComments(body))
	if m == nil || !explainableKeywords[strings.ToUpper(m[1])] {
		return "", "not a SELECT, WITH, VALUES, TABLE, INSERT, UPDATE, DELETE or MERGE"
	}
	return body, ""
}

// statementUnverifiable names why a valid statement cannot be compared: one
// that carries its own pg_hint_plan comment is planned with that hint on
// both sides.
func statementUnverifiable(body string) string {
	if strings.Contains(body, "/*+") {
		return "the statement carries its own pg_hint_plan comment"
	}
	return ""
}

// hasBindParam reports a $n outside literals and comments.
func hasBindParam(body string) bool {
	return bindParamRe.MatchString(workload.BlankLiteralsAndComments(body))
}

// planSummary is a plan's shape fingerprint and root total cost; disabled
// is set when the plan rests on a path the planner had disabled.
type planSummary struct {
	hash     string
	cost     float64
	disabled bool
}

// disableCost is the cost PostgreSQL 17 and older add to a disabled path;
// PostgreSQL 18 marks the node "Disabled" instead.
const disableCost = 1.0e10

func summarizePlan(planJSON []byte) (planSummary, error) {
	hash, err := planhash.Compute(planJSON)
	if err != nil {
		return planSummary{}, err
	}
	root, err := parsePlanRoot(planJSON)
	if err != nil {
		return planSummary{}, err
	}
	if root.NodeType == "" || math.IsNaN(root.TotalCost) {
		return planSummary{}, fmt.Errorf("plan without a root node")
	}
	return planSummary{hash: hash, cost: root.TotalCost,
		disabled: root.TotalCost >= disableCost || nodeDisabled(root)}, nil
}

func nodeDisabled(n planNode) bool {
	if n.Disabled {
		return true
	}
	for _, c := range n.Plans {
		if nodeDisabled(c) {
			return true
		}
	}
	return false
}

// judged summarizes both plans and judges the hint.
func judged(cmp HintComparison, plain, hinted []byte, hint string,
	opts CompareOptions) (HintComparison, error) {
	p, err := summarizePlan(plain)
	if err != nil {
		return refuse(cmp, HintExplainFailed, "plain plan: "+err.Error())
	}
	h, err := summarizePlan(hinted)
	if err != nil {
		return refuse(cmp, HintExplainFailed, "hinted plan: "+err.Error())
	}
	cmp.PlainPlan, cmp.HintedPlan = plain, hinted
	cmp.PlainHash, cmp.HintedHash = p.hash, h.hash
	cmp.PlainCost, cmp.HintedCost = p.cost, h.cost
	v, basis, reason := judgeHint(p, h, hint, opts.TolerancePct, planSpills(opts.SpillPlan))
	cmp.Basis = basis
	if v.Refused() {
		return refuse(cmp, v, reason)
	}
	cmp.Verdict, cmp.Reason = v, reason
	return cmp, nil
}

// judgeHint is the verdict of two planned sides (pure).
//   - a hinted plan on a disabled path (and a plain one not) has no effect.
//   - same shape: a hint of Set() directives with Set(work_mem) is admitted
//     (basis spill) when the cached ANALYZE plan spills, else unverifiable;
//     any other hint has no effect.
//   - changed shape: a hint with a join or scan directive whose cost rises
//     past plain x (1 + tolerance/100) is a cost regression; else changed.
func judgeHint(plain, hinted planSummary, hint string, tolerancePct float64,
	spill bool) (HintVerdict, string, string) {
	setOnly, workMem := hintDirectiveKinds(hint)
	if hinted.disabled && !plain.disabled {
		// pg_hint_plan 1.4-1.6 disable the other paths for an unknown index
		// or alias and the planner falls back to a disabled one.
		return HintNoEffect, "", "pg_hint_plan could not apply the hint: the hinted " +
			"plan rests on a disabled path (an unknown alias or index)"
	}
	if plain.hash == hinted.hash {
		if setOnly && workMem {
			if spill {
				return HintChanged, "spill", "the cached ANALYZE plan spills to disk"
			}
			return HintUnverifiable, "", "Set(work_mem) changes no plan shape and no " +
				"cached ANALYZE plan shows a spill"
		}
		return HintNoEffect, "", "the hinted plan is the plain plan (pg_hint_plan " +
			"ignored the hint or the planner already chose it)"
	}
	limit := plain.cost * (1 + tolerancePct/100)
	if !setOnly && hinted.cost > limit {
		return HintCostRegression, "plan", fmt.Sprintf("hinted cost %.2f exceeds plain "+
			"cost %.2f by more than %.1f%%", hinted.cost, plain.cost, tolerancePct)
	}
	return HintChanged, "plan", "the hint changes the plan"
}

// hintDirectiveKinds reports whether every directive is a Set() and
// whether one sets work_mem.
func hintDirectiveKinds(hint string) (setOnly, workMem bool) {
	setOnly = true
	for _, d := range splitHintDirectives(hint) {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if !strings.HasPrefix(d, "Set(") {
			setOnly = false
			continue
		}
		if m := setDirectiveRe.FindStringSubmatch(d); m != nil &&
			strings.EqualFold(m[1], "work_mem") {
			workMem = true
		}
	}
	return setOnly, workMem
}

// planSpills reports a disk sort or a multi-batch hash in a plan with
// actuals (EXPLAIN ANALYZE or auto_explain output).
func planSpills(planJSON []byte) bool {
	if len(planJSON) == 0 {
		return false
	}
	root, err := parsePlanRoot(planJSON)
	if err != nil {
		return false
	}
	return nodeSpills(root)
}

func nodeSpills(n planNode) bool {
	if n.SortSpaceType != nil && *n.SortSpaceType == "Disk" {
		return true
	}
	if n.HashBatches != nil && *n.HashBatches > 1 {
		return true
	}
	for _, c := range n.Plans {
		if nodeSpills(c) {
			return true
		}
	}
	return false
}

// Evidence is the finding detail hint_evidence: the verdict, both costs
// and plan hashes, the server major and both plans (each up to 64 KB).
func (c HintComparison) Evidence() map[string]any {
	ev := map[string]any{"verdict": string(c.Verdict), "reason": c.Reason,
		"pg_major": c.PGMajor, "generic_plan": c.Generic, "tolerance_pct": c.TolerancePct}
	if c.Basis != "" {
		ev["basis"] = c.Basis
	}
	if c.PlainHash == "" {
		return ev
	}
	ev["plain_cost"], ev["hinted_cost"] = c.PlainCost, c.HintedCost
	ev["plain_plan_hash"], ev["hinted_plan_hash"] = c.PlainHash, c.HintedHash
	if c.PlainCost > 0 {
		ev["cost_delta_pct"] = math.Round((c.HintedCost/c.PlainCost-1)*10000) / 100
	}
	addEvidencePlan(ev, "plain_plan", c.PlainPlan)
	addEvidencePlan(ev, "hinted_plan", c.HintedPlan)
	return ev
}

func addEvidencePlan(ev map[string]any, key string, plan []byte) {
	if len(plan) > maxEvidencePlanBytes {
		ev[key+"_omitted"] = fmt.Sprintf("plan of %d bytes exceeds %d", len(plan),
			maxEvidencePlanBytes)
		return
	}
	if json.Valid(plan) {
		ev[key] = json.RawMessage(plan)
	}
}
