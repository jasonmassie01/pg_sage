package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// `approve <change-id>` (cli-01a) prints the change's approval card with
// its card hash and, after --yes or the typed database name, posts that
// hash to the approval route, so an edit made since it was shown is
// refused. The sidecar caps a token approval by
// notification_policy.chat_approval_max_tier and refuses agent tokens.
// `approve --plan <id>` approves a plan (all steps, or --up-to N) bound to
// the plan hash it printed. Exit 0 approved, 1 refused, 2 usage, 3 auth.

const approveUsage = `usage: pg_sage_sidecar approve <change-id> [--yes] [--database D] [--url U]
       pg_sage_sidecar approve --plan <plan-id> [--up-to N] [--yes] [--database D] [--url U]
  Prints the approval card (or the plan) with its hash, then approves that
  hash after --yes or the typed database name. High-risk changes, and any
  above notification_policy.chat_approval_max_tier, are approved in pg_sage.
  Exit 0 approved, 1 refused, 2 usage, 3 auth.`

// decisionArgs are approve's and apply's parsed arguments.
type decisionArgs struct {
	cliFlags
	id     int64
	plan   bool
	yes    bool
	upTo   int
	upToOn bool
}

// parseDecisionArgs reads `<id> [flags]` or `--plan <id> [flags]`.
func parseDecisionArgs(name, usage string, args []string, env cliEnv) (decisionArgs, int,
	bool) {
	var a decisionArgs
	var planID string
	fs := newFlagSet(name)
	a.register(fs)
	fs.BoolVar(&a.yes, "yes", false, "skip the confirmation")
	fs.StringVar(&planID, "plan", "", "a plan id instead of a change id")
	if name == "approve" {
		fs.IntVar(&a.upTo, "up-to", 0, "approve plan steps 1..N")
	}
	var positional []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = append(positional, args[0]), args[1:]
	}
	if code, done := parseCLIFlags(fs, args, usage, planExitUsage, env); done {
		return a, code, true
	}
	positional = append(positional, fs.Args()...)
	fs.Visit(func(f *flag.Flag) { a.upToOn = a.upToOn || f.Name == "up-to" })
	raw, what := "", "change id"
	switch {
	case planID != "" && len(positional) == 0:
		raw, what, a.plan = planID, "plan id", true
	case planID == "" && len(positional) == 1:
		raw = positional[0]
	default:
		return a, usageError(env, name, usage, planExitUsage,
			"give one change id, or --plan and one plan id"), true
	}
	id, err := positiveID(raw, what)
	if err != nil {
		return a, usageError(env, name, usage, planExitUsage, "%v", err), true
	}
	a.id = id
	if a.upToOn && (!a.plan || a.upTo < 1) {
		return a, usageError(env, name, usage, planExitUsage,
			"--up-to takes a plan step number from 1, with --plan"), true
	}
	return a, 0, false
}

func runApprove(ctx context.Context, args []string, env cliEnv) int {
	a, code, done := parseDecisionArgs("approve", approveUsage, args, env)
	if done {
		return code
	}
	s, code := connect("approve", a.cliFlags, env, planExitAuth)
	if s == nil {
		return code
	}
	if a.plan {
		return s.approvePlan(ctx, a)
	}
	return s.approveChange(ctx, a)
}

func dbQuery(database string) url.Values {
	q := url.Values{}
	if database != "" {
		q.Set("database", database)
	}
	return q
}

// approveChange prints the card, confirms, and posts the card hash once.
func (s *cliSession) approveChange(ctx context.Context, a decisionArgs) int {
	path := fmt.Sprintf("/api/v1/approvals/%d", a.id)
	ans, err := s.api.send(ctx, http.MethodGet, path, dbQuery(a.database), nil)
	if err != nil {
		return s.fail(err, planExitAuth)
	}
	card, _ := ans.body["card"].(map[string]any)
	hash, _ := card["card_hash"].(string)
	database, _ := card["database"].(string)
	printCard(s.env, card)
	if hash == "" {
		_, _ = fmt.Fprintf(s.env.stderr, "approve: change %d has no card hash\n", a.id)
		return planExitRefused
	}
	if !s.confirm(a.yes, database, fmt.Sprintf("approve change %d", a.id)) {
		return planExitRefused
	}
	ans, err = s.api.send(ctx, http.MethodPost, path+"/approve", dbQuery(a.database),
		map[string]any{"card_hash": hash})
	if err != nil {
		return s.fail(err, planExitAuth)
	}
	if a.json {
		_, _ = fmt.Fprintln(s.env.stdout, s.api.redact(indentJSON(ans.raw)))
		return planExitOK
	}
	b := ans.body
	switch {
	case b["status"] == "scheduled":
		_, _ = fmt.Fprintf(s.env.stdout, "change %d on %s: approved for its window; it runs "+
			"from %v\n", a.id, database, b["run_after_label"])
	case b["executed"] == true:
		_, _ = fmt.Fprintf(s.env.stdout, "change %d on %s: approved and run (verification "+
			"%v, action log %v)\n", a.id, database, b["verification_status"], b["action_log_id"])
	default:
		_, _ = fmt.Fprintf(s.env.stdout, "change %d on %s: approved; the run failed: %v\n",
			a.id, database, b["error"])
		return planExitRefused
	}
	return planExitOK
}

func printCard(env cliEnv, card map[string]any) {
	risk, _ := card["risk"].(map[string]any)
	_, _ = fmt.Fprintf(env.stdout, "change %v on %v: %v\n  action: %v  risk: %v  status: %v\n"+
		"  sql: %v\n  card hash: %v\n", card["queue_id"], card["database"], card["title"],
		card["action_type"], risk["tier"], card["status"], card["sql"], card["card_hash"])
}

// approvePlan prints the plan with its hash, confirms, and approves it.
func (s *cliSession) approvePlan(ctx context.Context, a decisionArgs) int {
	detail, code := s.planDetail(ctx, a)
	if detail == nil {
		return code
	}
	hash, _ := detail["plan_hash"].(string)
	database, _ := detail["database"].(string)
	if !s.confirm(a.yes, database, fmt.Sprintf("approve plan %d", a.id)) {
		return planExitRefused
	}
	body := map[string]any{"plan_hash": hash, "mode": "all", "via": "cli"}
	if a.upToOn {
		body["mode"], body["up_to"] = "up_to", a.upTo
	}
	ans, err := s.api.send(ctx, http.MethodPost, fmt.Sprintf("/api/v1/plans/%d/approve",
		a.id), dbQuery(a.database), body)
	if err != nil {
		return s.fail(err, planExitAuth)
	}
	printPlanDecision(s.env.stdout, "approve", planArgs{id: a.id, upTo: a.upTo}, ans.body)
	return planExitOK
}

// planDetail reads and prints a plan; nil is a failure with its code.
func (s *cliSession) planDetail(ctx context.Context, a decisionArgs) (map[string]any, int) {
	ans, err := s.api.send(ctx, http.MethodGet, fmt.Sprintf("/api/v1/plans/%d", a.id),
		dbQuery(a.database), nil)
	if err != nil {
		return nil, s.fail(err, planExitAuth)
	}
	printPlanDetail(s.env.stdout, ans.body)
	if hash, _ := ans.body["plan_hash"].(string); hash == "" {
		_, _ = fmt.Fprintf(s.env.stderr, "%s: plan %d has no plan hash\n", s.name, a.id)
		return nil, planExitRefused
	}
	return ans.body, planExitOK
}
