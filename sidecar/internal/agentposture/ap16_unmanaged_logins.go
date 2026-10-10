package agentposture

import (
	"context"
	"fmt"
)

func init() { Register(ap16{}) }

// ap16 reports agent-like logins Guard does not manage once any Guard
// principal exists (SR-59): roles seen with an agent-like
// application_name (the AP-01 hints, which include the agent-like
// clients of AP-13's shared logins) that are not Guard agent roles and
// can log in. Before the first principal, AP-01 and AP-13 report them;
// after it, an agent outside Guard is a gap in its coverage. Warning.
type ap16 struct{}

func (ap16) Spec() Spec {
	return Spec{ID: "AP-16", Title: "Agent-like logins outside Agent Guard", Severity: Warning}
}

var ap16SQL = Statement("AP-16", `SELECT r.oid, r.rolname::text
FROM pg_catalog.pg_roles r
WHERE r.oid = ANY($1::oid[]) AND r.rolcanlogin
ORDER BY 2
LIMIT $2`)

func (ap16) Detect(ctx context.Context, in Input) ([]Finding, error) {
	hints := in.Env.HintAgents()
	if !in.Env.PrincipalsExist || len(hints) == 0 {
		return nil, nil
	}
	oids := make([]uint32, len(hints))
	for i, h := range hints {
		oids[i] = h.OID
	}
	rows, err := in.Q.Query(ctx, ap16SQL, oids, apbMaxRows)
	if err != nil {
		return nil, fmt.Errorf("read agent-like logins: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var oid uint32
		var name string
		if err := rows.Scan(&oid, &name); err != nil {
			return nil, fmt.Errorf("read agent-like logins: %w", err)
		}
		hint, _ := in.Env.Agent(oid)
		out = append(out, unmanagedLogin(name, hint.Hint))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read agent-like logins: %w", err)
	}
	return out, nil
}

func unmanagedLogin(name, app string) Finding {
	seen := "an agent-like client"
	if app != "" {
		seen = "application_name " + app
	}
	return Finding{Severity: Warning, ObjectType: "role", Object: name,
		Title: "Agent-like login outside Agent Guard",
		Detail: "Login " + name + " is used by " + seen + " but is not a Guard agent " +
			"role: Guard's grants, limits, audit and kill switch do not cover it.",
		Recommendation: "Register the agent as a Guard principal and move it to its " +
			"Guard credentials, then stop this login.",
		FixScript: "-- after the agent uses its Guard credentials:\nALTER ROLE " +
			QuoteIdent(name) + " NOLOGIN;",
		Caveat: "Agent-like is a hint from application_name (agents.client_patterns); " +
			"an application can name itself anything.",
		Evidence: []Evidence{{Source: "pg_stat_activity", Ref: name, Detail: seen}}}
}
