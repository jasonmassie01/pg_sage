package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The plan command's requests and output (plan_cmd.go).

// planAnswer is one sidecar answer (cli_client.go).
type planAnswer = apiAnswer

// run performs cmd and prints its answer; the exit code follows it.
func (c *planClient) run(ctx context.Context, cmd string, a planArgs) int {
	if cmd == "approve" && a.step > 0 {
		return c.approveStep(ctx, a)
	}
	method, path, body := http.MethodPost, fmt.Sprintf("/api/v1/plans/%d/%s", a.id, cmd), any(nil)
	switch cmd {
	case "list":
		method, path = http.MethodGet, "/api/v1/plans"
	case "show":
		method, path = http.MethodGet, fmt.Sprintf("/api/v1/plans/%d", a.id)
	case "approve":
		body = approveBody(a)
	case "resume":
		body = map[string]any{"plan_hash": strings.TrimSpace(a.hash)}
	case "reject", "cancel":
		body = map[string]any{"reason": a.reason}
	}
	ans, code := c.call(ctx, method, path, a, body)
	if code != planExitOK {
		return code
	}
	c.print(cmd, a, ans)
	return planExitOK
}

func approveBody(a planArgs) map[string]any {
	b := map[string]any{"plan_hash": strings.TrimSpace(a.hash), "mode": "all", "via": "cli"}
	if a.upTo > 0 {
		b["mode"], b["up_to"] = "up_to", a.upTo
	}
	return b
}

// approveStep approves step ordinal a.step: the plan's detail names its
// step id.
func (c *planClient) approveStep(ctx context.Context, a planArgs) int {
	ans, code := c.call(ctx, http.MethodGet, fmt.Sprintf("/api/v1/plans/%d", a.id), a, nil)
	if code != planExitOK {
		return code
	}
	plan, _ := ans.body["plan"].(map[string]any)
	steps, _ := plan["steps"].([]any)
	var stepID int64
	for _, s := range steps {
		st, _ := s.(map[string]any)
		if ord, _ := st["ordinal"].(float64); int(ord) == a.step {
			id, _ := st["id"].(float64)
			stepID = int64(id)
		}
	}
	if stepID == 0 {
		_, _ = fmt.Fprintf(c.stderr, "plan: plan %d has no step %d\n", a.id, a.step)
		return planExitRefused
	}
	path := fmt.Sprintf("/api/v1/plans/%d/steps/%d/approve", a.id, stepID)
	ans, code = c.call(ctx, http.MethodPost, path, a,
		map[string]any{"plan_hash": strings.TrimSpace(a.hash), "via": "cli"})
	if code != planExitOK {
		return code
	}
	c.print("approve", a, ans)
	return planExitOK
}

// call sends one request and maps a refusal to its exit code, printing
// the sidecar's message.
func (c *planClient) call(ctx context.Context, method, path string, a planArgs,
	body any) (planAnswer, int) {
	q := url.Values{}
	if a.database != "" {
		q.Set("database", a.database)
	}
	if a.status != "" {
		q.Set("status", a.status)
	}
	ans, err := c.api.send(ctx, method, path, q, body)
	var refusal *cliRefusal
	var transport *cliTransportError
	switch {
	case err == nil:
		return ans, planExitOK
	case errors.As(err, &transport):
		_, _ = fmt.Fprintf(c.stderr, "plan: %v\n", transport)
		return ans, planExitRefused
	case errors.As(err, &refusal):
		_, _ = fmt.Fprintf(c.stderr, "plan: %v\n", refusal)
		if refusal.status == http.StatusUnauthorized || refusal.status == http.StatusForbidden {
			return ans, planExitAuth
		}
		return ans, planExitRefused
	}
	_, _ = fmt.Fprintf(c.stderr, "plan: %v\n", err)
	return ans, planExitUsage
}

// print writes the answer: the sidecar's JSON with --json, else a summary.
func (c *planClient) print(cmd string, a planArgs, ans planAnswer) {
	if a.json {
		_, _ = fmt.Fprintln(c.stdout, c.api.redact(indentJSON(ans.raw)))
		return
	}
	switch cmd {
	case "list":
		printPlanList(c.stdout, ans.body)
	case "show":
		printPlanDetail(c.stdout, ans.body)
	default:
		printPlanDecision(c.stdout, cmd, a, ans.body)
	}
}

func printPlanList(w io.Writer, body map[string]any) {
	plans, _ := body["plans"].([]any)
	_, _ = fmt.Fprintf(w, "%-6s %-16s %-10s %-14s %s\n", "ID", "DATABASE", "STATUS", "HASH",
		"GOAL")
	for _, p := range plans {
		m, _ := p.(map[string]any)
		_, _ = fmt.Fprintf(w, "%-6v %-16v %-10v %-14s %v\n", m["id"], m["database"],
			m["status"], short(m["plan_hash"]), m["goal"])
	}
	if errs, _ := body["errors"].([]any); len(errs) > 0 {
		_, _ = fmt.Fprintf(w, "(%d database(s) could not be read)\n", len(errs))
	}
}

func printPlanDetail(w io.Writer, body map[string]any) {
	plan, _ := body["plan"].(map[string]any)
	_, _ = fmt.Fprintf(w, "plan %v on %v: %v\nstatus %v, hash %v\n", plan["id"],
		body["database"], plan["goal"], plan["status"], body["plan_hash"])
	coverage := map[float64]map[string]any{}
	for _, c := range listOf(body["coverage"]) {
		coverage[toFloat(c["step_id"])] = c
	}
	steps, _ := plan["steps"].([]any)
	for _, s := range steps {
		st, _ := s.(map[string]any)
		runtime, _ := st["runtime"].(map[string]any)
		cov := coverage[toFloat(st["id"])]
		gate := "approved"
		if cov["covered"] != true {
			gate = fmt.Sprintf("needs approval: %v", cov["reason"])
		}
		sql, _ := st["proposed_sql"].(string)
		_, _ = fmt.Fprintf(w, "  %v. [%v] %v (%s)\n", st["ordinal"], runtime["state"],
			firstLine(sql), gate)
	}
}

func printPlanDecision(w io.Writer, cmd string, a planArgs, body map[string]any) {
	switch {
	case cmd == "approve" && a.step > 0:
		_, _ = fmt.Fprintf(w, "plan %d on %v: step %d approved\n", a.id, body["database"],
			a.step)
	case cmd == "approve":
		ap, _ := body["approval"].(map[string]any)
		mode := fmt.Sprint(ap["mode"])
		if up, ok := ap["up_to"]; ok {
			mode += fmt.Sprintf(" %v", up)
		}
		_, _ = fmt.Fprintf(w, "plan %d on %v: approved (%s, approval %v)\n", a.id,
			body["database"], mode, ap["id"])
	default:
		_, _ = fmt.Fprintf(w, "plan %d on %v: %v\n", a.id, body["database"], body["status"])
	}
}

func listOf(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func toFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

func short(v any) string {
	s, _ := v.(string)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 80 {
		return line[:80] + "..."
	}
	return line
}
