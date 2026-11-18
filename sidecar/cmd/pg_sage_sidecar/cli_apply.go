package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// `apply` (cli-01a) adds no execution route. `apply --plan <id>` resumes
// a halted plan (plan-03a's resume route, bound to the plan hash it
// printed); an approved plan is already run step by step by the plan
// engine. `apply <change-id>` reports an approved change: outside its
// maintenance window it exits 1 naming the next window and the why-not
// chain's window link; inside it, the executor's own scheduler runs it.

const applyUsage = `usage: pg_sage_sidecar apply <change-id> [--database D] [--url U] [--json]
       pg_sage_sidecar apply --plan <plan-id> [--yes] [--database D] [--url U]
  A change: says when the executor's scheduler runs it (exit 1 outside its
  window, naming the next one). A plan: resumes a halted plan after --yes
  or the typed database name; an approved plan already runs step by step.
  Exit 0 applied or due, 1 refused or not yet, 2 usage, 3 auth.`

// changeEnded are the statuses with nothing left to apply.
var changeEnded = map[string]bool{"rejected": true, "expired": true, "failed": true,
	"cancelled": true, "superseded": true}

func runApply(ctx context.Context, args []string, env cliEnv) int {
	a, code, done := parseDecisionArgs("apply", applyUsage, args, env)
	if done {
		return code
	}
	s, code := connect("apply", a.cliFlags, env, planExitAuth)
	if s == nil {
		return code
	}
	if a.plan {
		return s.applyPlan(ctx, a)
	}
	return s.applyChange(ctx, a)
}

func (s *cliSession) applyChange(ctx context.Context, a decisionArgs) int {
	ans, err := s.api.send(ctx, http.MethodGet, fmt.Sprintf("/api/v1/approvals/%d", a.id),
		dbQuery(a.database), nil)
	if err != nil {
		return s.fail(err, planExitAuth)
	}
	if a.json {
		_, _ = fmt.Fprintln(s.env.stdout, s.api.redact(indentJSON(ans.raw)))
	}
	card, _ := ans.body["card"].(map[string]any)
	status, _ := card["status"].(string)
	db := fmt.Sprint(card["database"])
	switch {
	case status == "pending":
		_, _ = fmt.Fprintf(s.env.stderr, "apply: change %d on %s is awaiting approval: run "+
			"approve %d\n", a.id, db, a.id)
		return planExitRefused
	case status == "scheduled":
		return s.applyScheduled(ctx, a.id, card, ans.body)
	case changeEnded[status]:
		_, _ = fmt.Fprintf(s.env.stderr, "apply: change %d on %s is %s; nothing to apply\n",
			a.id, db, status)
		return planExitRefused
	}
	_, _ = fmt.Fprintf(s.env.stdout, "change %d on %s is %s\n", a.id, db, status)
	return planExitOK
}

// applyScheduled reports a change approved for its window: due, or
// outside it with the next window and the why-not window link.
func (s *cliSession) applyScheduled(ctx context.Context, id int64, card,
	body map[string]any) int {
	sched, _ := body["schedule"].(map[string]any)
	db := fmt.Sprint(card["database"])
	if sched["due"] == true {
		_, _ = fmt.Fprintf(s.env.stdout, "change %d on %s is due: its window is open and the "+
			"executor's scheduler runs it\n", id, db)
		return planExitOK
	}
	_, _ = fmt.Fprintf(s.env.stdout, "change %d on %s is approved for its window and outside "+
		"it: the next window opens %v (%v)\n", id, db, sched["run_after_label"],
		sched["run_after"])
	finding, _ := card["finding"].(map[string]any)
	if fid := toFloat(finding["id"]); fid > 0 {
		q := url.Values{"database": {db}, "finding_id": {fmt.Sprintf("%d", int64(fid))}}
		if ans, err := s.api.send(ctx, http.MethodGet, "/api/v1/whynot", q, nil); err == nil {
			for _, l := range listOf(ans.body["links"]) {
				if l["name"] == "window" {
					_, _ = fmt.Fprintf(s.env.stdout, "  why not: window (%v): %v\n", l["status"],
						l["sentence"])
				}
			}
		}
	}
	return planExitRefused
}

func (s *cliSession) applyPlan(ctx context.Context, a decisionArgs) int {
	detail, code := s.planDetail(ctx, a)
	if detail == nil {
		return code
	}
	plan, _ := detail["plan"].(map[string]any)
	status, _ := plan["status"].(string)
	db := fmt.Sprint(detail["database"])
	switch status {
	case "halted":
		return s.resumePlan(ctx, a, detail)
	case "approved", "running":
		_, _ = fmt.Fprintf(s.env.stdout, "plan %d on %s is %s: the plan engine runs it step "+
			"by step\n", a.id, db, status)
		return planExitOK
	case "completed":
		_, _ = fmt.Fprintf(s.env.stdout, "plan %d on %s completed\n", a.id, db)
		return planExitOK
	case "draft", "proposed":
		_, _ = fmt.Fprintf(s.env.stderr, "apply: plan %d on %s awaits approval: run approve "+
			"--plan %d\n", a.id, db, a.id)
		return planExitRefused
	}
	_, _ = fmt.Fprintf(s.env.stderr, "apply: plan %d on %s is %s; nothing to apply\n", a.id,
		db, status)
	return planExitRefused
}

// resumePlan resumes a halted plan bound to the hash it printed.
func (s *cliSession) resumePlan(ctx context.Context, a decisionArgs,
	detail map[string]any) int {
	db, _ := detail["database"].(string)
	if !s.confirm(a.yes, db, fmt.Sprintf("resume plan %d", a.id)) {
		return planExitRefused
	}
	ans, err := s.api.send(ctx, http.MethodPost, fmt.Sprintf("/api/v1/plans/%d/resume", a.id),
		dbQuery(a.database), map[string]any{"plan_hash": detail["plan_hash"]})
	if err != nil {
		return s.fail(err, planExitAuth)
	}
	printPlanDecision(s.env.stdout, "resume", planArgs{id: a.id}, ans.body)
	return planExitOK
}
