package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// `plan` with no verb (cli-01a): what needs a person now. It lists, in
// order, the Inbox items waiting on a person (ux-01a), the plans and
// changes due (an approved or running plan, a change approved for its
// maintenance window), and the held-back changes with their first why-not
// link (ux-08a). It only reads. Exit 0 nothing needs a person, 2 something
// does, 1 an error (an Inbox source that could not be read is an error,
// never "nothing"), 3 auth, 64 usage.

const planStatusUsage = `usage: pg_sage_sidecar plan [--database D] [--url U] [--json]
  With no command: list what needs a person now (Inbox items), the plans
  and changes due, and held-back changes with their first why-not link.
  Exit 0 nothing needs you, 2 something does, 1 error, 3 auth, 64 usage.
` + planUsage

// maxWhyNotLookups bounds the why-not reads one status run makes.
const maxWhyNotLookups = 10

// planStatus is the stable --json form.
type planStatus struct {
	NeedsPerson bool             `json:"needs_person"`
	Waiting     []map[string]any `json:"waiting"`
	Due         []statusDue      `json:"due"`
	HeldBack    []statusHeld     `json:"held_back"`
}

// statusDue is a plan or change that runs without a person.
type statusDue struct {
	Kind     string `json:"kind"`
	ID       any    `json:"id"`
	Database any    `json:"database"`
	Status   any    `json:"status"`
	What     any    `json:"what"`
}

// statusHeld is a pending change pg_sage holds back, with the first
// failing link of its why-not chain.
type statusHeld struct {
	ID       any    `json:"id"`
	Database any    `json:"database"`
	Reason   any    `json:"reason"`
	WhyNot   string `json:"why_not,omitempty"`
	Sentence string `json:"why_not_sentence,omitempty"`
}

func runPlanStatus(ctx context.Context, args []string, env cliEnv) int {
	var f cliFlags
	fs := newFlagSet("plan")
	f.register(fs)
	if code, done := parseCLIFlags(fs, args, planStatusUsage, statusExitUsage, env); done {
		return code
	}
	if fs.NArg() > 0 {
		return usageError(env, "plan", planStatusUsage, statusExitUsage,
			"unexpected argument %q", fs.Arg(0))
	}
	s, code := connect("plan", f, env, statusExitAuth)
	if s == nil {
		return code
	}
	st, code := s.readStatus(ctx, f.database)
	if code != statusExitClear {
		return code
	}
	if f.json {
		raw, _ := json.MarshalIndent(st, "", "  ")
		_, _ = fmt.Fprintln(env.stdout, s.api.redact(string(raw)))
	} else {
		printPlanStatus(env, st)
	}
	if st.NeedsPerson {
		return statusExitNeedsPerson
	}
	return statusExitClear
}

// readStatus reads the Inbox, the plans and the action queue.
func (s *cliSession) readStatus(ctx context.Context, database string) (planStatus, int) {
	q := url.Values{}
	if database != "" {
		q.Set("database", database)
	}
	var st planStatus
	inbox, err := s.api.send(ctx, http.MethodGet, "/api/v1/inbox", q, nil)
	if err != nil {
		return st, s.fail(err, statusExitAuth)
	}
	st.Waiting = listOf(inbox.body["items"])
	if len(st.Waiting) == 0 && inbox.body["lower_bound"] == true {
		_, _ = fmt.Fprintf(s.env.stderr, "plan: the Inbox could not read every source, so "+
			"it cannot say nothing needs you: %s\n", sourceErrors(inbox.body))
		return st, statusExitError
	}
	st.NeedsPerson = len(st.Waiting) > 0
	plans, err := s.api.send(ctx, http.MethodGet, "/api/v1/plans", q, nil)
	if err != nil {
		return st, s.fail(err, statusExitAuth)
	}
	actions, err := s.api.send(ctx, http.MethodGet, "/api/v1/actions/pending", q, nil)
	if err != nil {
		return st, s.fail(err, statusExitAuth)
	}
	st.Due = duePlans(plans.body)
	st.Due = append(st.Due, dueChanges(actions.body)...)
	st.HeldBack = s.heldBack(ctx, actions.body)
	return st, statusExitClear
}

func sourceErrors(body map[string]any) string {
	parts := []string{}
	for _, e := range listOf(body["errors"]) {
		parts = append(parts, fmt.Sprintf("%v on %v: %v", e["source"], e["database"],
			e["error"]))
	}
	return strings.Join(parts, "; ")
}

// duePlans are the approved and running plans: the plan engine runs them.
func duePlans(body map[string]any) []statusDue {
	out := []statusDue{}
	for _, p := range listOf(body["plans"]) {
		if p["status"] == "approved" || p["status"] == "running" {
			out = append(out, statusDue{Kind: "plan", ID: p["id"], Database: p["database"],
				Status: p["status"], What: p["goal"]})
		}
	}
	return out
}

// dueChanges are the changes approved for their maintenance window.
func dueChanges(body map[string]any) []statusDue {
	out := []statusDue{}
	for _, a := range listOf(body["scheduled"]) {
		when := a["run_after_label"]
		if when == nil || when == "" {
			when = a["run_after"]
		}
		out = append(out, statusDue{Kind: "change", ID: a["id"], Database: a["database_name"],
			Status: "scheduled", What: when})
	}
	return out
}

// heldBack lists the pending changes that are not eligible now, each with
// the first failing link of its why-not chain (up to maxWhyNotLookups).
func (s *cliSession) heldBack(ctx context.Context, body map[string]any) []statusHeld {
	out := []statusHeld{}
	for _, a := range listOf(body["pending"]) {
		if a["eligible"] != false {
			continue
		}
		h := statusHeld{ID: a["id"], Database: a["database_name"], Reason: a["defer_reason"]}
		if len(out) < maxWhyNotLookups {
			h.WhyNot, h.Sentence = s.firstWhyNot(ctx, a)
		}
		out = append(out, h)
	}
	return out
}

// firstWhyNot is the first failing link of a pending change's finding, or
// "" when it cannot be read (the held-back line still prints).
func (s *cliSession) firstWhyNot(ctx context.Context, action map[string]any) (string, string) {
	finding := toFloat(action["finding_id"])
	db, _ := action["database_name"].(string)
	if finding <= 0 || db == "" {
		return "", ""
	}
	q := url.Values{"database": {db}, "finding_id": {fmt.Sprintf("%d", int64(finding))}}
	ans, err := s.api.send(ctx, http.MethodGet, "/api/v1/whynot", q, nil)
	if err != nil {
		return "", ""
	}
	first, _ := ans.body["first_fail"].(string)
	for _, l := range listOf(ans.body["links"]) {
		if l["name"] == first {
			sentence, _ := l["sentence"].(string)
			return first, sentence
		}
	}
	return first, ""
}

func printPlanStatus(env cliEnv, st planStatus) {
	w := env.stdout
	if !st.NeedsPerson && len(st.Due) == 0 && len(st.HeldBack) == 0 {
		_, _ = fmt.Fprintln(w, "Nothing needs you.")
		return
	}
	if st.NeedsPerson {
		_, _ = fmt.Fprintf(w, "Waiting on you (%d):\n", len(st.Waiting))
		for _, it := range st.Waiting {
			_, _ = fmt.Fprintf(w, "  %-18v %-6v %-14v %v [%v] %v\n", it["kind"], it["id"],
				it["database"], it["title"], it["risk"], it["href"])
		}
	} else {
		_, _ = fmt.Fprintln(w, "Nothing needs you.")
	}
	if len(st.Due) > 0 {
		_, _ = fmt.Fprintln(w, "Due (runs without you):")
		for _, d := range st.Due {
			_, _ = fmt.Fprintf(w, "  %s %v on %v: %v (%v)\n", d.Kind, d.ID, d.Database,
				d.Status, d.What)
		}
	}
	if len(st.HeldBack) > 0 {
		_, _ = fmt.Fprintln(w, "Held back:")
		for _, h := range st.HeldBack {
			_, _ = fmt.Fprintf(w, "  change %v on %v: %v\n", h.ID, h.Database, h.Reason)
			if h.WhyNot != "" {
				_, _ = fmt.Fprintf(w, "    why not: %s: %s\n", h.WhyNot, h.Sentence)
			}
		}
	}
}
