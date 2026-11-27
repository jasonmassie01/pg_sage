package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// `explain` (cli-01a) prints ux-08a's why-not chain from GET
// /api/v1/whynot: the ten links in chain order, each with its status,
// sentence and fix, and the first failing link. It only reads.

const explainUsage = `usage: pg_sage_sidecar explain --database D [--finding N | --object S.T]
                               [--url U] [--json]
  Prints why pg_sage has not acted: the why-not chain's ten links, from
  observing the database to the emergency stop, with the first failing one.
  Exit 0 printed, 1 refused, 2 usage, 3 auth.`

// explainQuery validates the flags into the why-not query; "" is valid.
func explainQuery(database, finding, object string, extra []string) (url.Values, string) {
	switch {
	case len(extra) > 0:
		return nil, fmt.Sprintf("unexpected argument %q", extra[0])
	case database == "":
		return nil, "--database is required"
	case finding != "" && object != "":
		return nil, "give --finding or --object, not both"
	}
	q := url.Values{"database": {database}}
	if finding != "" {
		id, err := positiveID(finding, "finding id")
		if err != nil {
			return nil, err.Error()
		}
		q.Set("finding_id", strconv.FormatInt(id, 10))
	}
	if object != "" {
		q.Set("object", object)
	}
	return q, ""
}

func runExplain(ctx context.Context, args []string, env cliEnv) int {
	var f cliFlags
	var finding, object string
	fs := newFlagSet("explain")
	f.register(fs)
	fs.StringVar(&finding, "finding", "", "a finding id")
	fs.StringVar(&object, "object", "", "an object, schema.name")
	if code, done := parseCLIFlags(fs, args, explainUsage, planExitUsage, env); done {
		return code
	}
	q, problem := explainQuery(f.database, finding, object, fs.Args())
	if problem != "" {
		return usageError(env, "explain", explainUsage, planExitUsage, "%s", problem)
	}
	s, code := connect("explain", f, env, planExitAuth)
	if s == nil {
		return code
	}
	ans, err := s.api.send(ctx, http.MethodGet, "/api/v1/whynot", q, nil)
	if err != nil {
		return s.fail(err, planExitAuth)
	}
	if f.json {
		_, _ = fmt.Fprintln(env.stdout, s.api.redact(indentJSON(ans.raw)))
		return planExitOK
	}
	printWhyNot(env, ans.body)
	return planExitOK
}

func printWhyNot(env cliEnv, body map[string]any) {
	w := env.stdout
	subject := "the whole database"
	if body["finding_id"] != nil {
		subject = fmt.Sprintf("finding %v", body["finding_id"])
	} else if body["object"] != nil {
		subject = fmt.Sprint(body["object"])
	}
	_, _ = fmt.Fprintf(w, "why not, on %v for %s:\n", body["database"], subject)
	for i, l := range listOf(body["links"]) {
		mark := " "
		if l["first_fail"] == true {
			mark = "*"
		}
		_, _ = fmt.Fprintf(w, "%2d. %-17v %-14v%s %v\n", i+1, l["name"], l["status"], mark,
			l["sentence"])
		if fix, ok := l["fix"].(map[string]any); ok {
			_, _ = fmt.Fprintf(w, "      fix (%v): %v %v\n", fix["kind"], fix["value"],
				fix["note"])
		} else if l["no_fix"] != nil {
			_, _ = fmt.Fprintf(w, "      no fix: %v\n", l["no_fix"])
		}
	}
	if first, _ := body["first_fail"].(string); first != "" {
		_, _ = fmt.Fprintf(w, "first failing link: %s\n", first)
	} else {
		_, _ = fmt.Fprintln(w, "no link fails")
	}
}
