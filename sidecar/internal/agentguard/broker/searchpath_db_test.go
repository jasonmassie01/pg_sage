//go:build cgo

package broker

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// The search path is pg_catalog, the schemas the agent's broker role holds
// USAGE on by a grant of its own (sorted), then pg_temp (spec §6.8). A
// schema it reaches only through PUBLIC (public itself, by default) is
// never on it, so an object an attacker plants there under an unqualified
// name the agent uses is never what the name resolves to.
func TestSearchPathIsTheAgentsGrantedSchemas(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	suffix := f.schema[3:]
	second := f.schema + "_b"
	shadow, fn := "shadow_"+suffix, "planted_"+suffix
	exec(t, f.super, "CREATE SCHEMA "+ident(second))
	exec(t, f.super, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", ident(second),
		ident(f.role)))
	exec(t, f.super, fmt.Sprintf("CREATE TABLE public.%s (secret text); "+
		"INSERT INTO public.%s VALUES ('planted'); GRANT SELECT ON public.%s TO PUBLIC",
		shadow, shadow, shadow))
	exec(t, f.super, fmt.Sprintf("CREATE FUNCTION public.%s() RETURNS int "+
		"LANGUAGE sql STABLE AS 'SELECT 1/0'", fn))
	t.Cleanup(func() {
		for _, s := range []string{"DROP SCHEMA IF EXISTS " + ident(second) + " CASCADE",
			"DROP TABLE IF EXISTS public." + shadow,
			"DROP FUNCTION IF EXISTS public." + fn + "()"} {
			_, _ = f.super.Exec(t.Context(), s)
		}
	})

	res := f.query(t, "SELECT current_setting('search_path')")
	first, last := f.schema, second
	if want := "pg_catalog, " + first + ", " + last + ", pg_temp"; res.Status != StatusOK ||
		cell(t, res, 0, 0) != want {
		t.Fatalf("search_path = %+v, want %s", res, want)
	}
	res = f.query(t, "SELECT secret FROM "+shadow)
	if res.Status == StatusOK || res.Verdict == VerdictExecute && res.SQLState != "42P01" {
		t.Errorf("planted table in public = %+v, want it never resolved", res)
	}
	res = f.query(t, "SELECT "+fn+"()")
	if res.Status == StatusOK || res.SQLState == "22012" {
		t.Errorf("planted function in public = %+v, want it never resolved or run", res)
	}
	// Qualified, the agent reaches public only as far as PUBLIC's grants do;
	// that is the PUBLIC baseline, not the search path.
	exec(t, f.super, fmt.Sprintf("REVOKE USAGE ON SCHEMA %s, %s FROM %s",
		ident(f.schema), ident(second), ident(f.role)))
	res = f.query(t, "SELECT current_setting('search_path')")
	if res.Status != StatusOK || cell(t, res, 0, 0) != "pg_catalog, pg_temp" {
		t.Errorf("no granted schema: search_path = %+v, want pg_catalog, pg_temp", res)
	}
}

// D9: a parked verdict (agent_rate) is a refusal that carries when to retry.
func TestQueryRateLimitedCarriesRetryAfter(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.decider.verdict = decide.Verdict{Reason: decide.ReasonRate, Step: "D9", Park: true,
		RetryAfter: 90 * time.Second, Detail: "more than 30 requests in an hour"}
	res := f.query(t, "SELECT id FROM items")
	if res.Verdict != VerdictBlocked || res.ReasonCode != "agent_rate" ||
		res.RetryAfterSeconds != 90 || len(res.Rows) != 0 {
		t.Errorf("rate limited = %+v, want blocked agent_rate retry 90s", res)
	}
	if f.logins.calls != 0 {
		t.Error("a parked request opened a broker login")
	}
	if a := f.auditRows(t); len(a) != 1 || a[0].reason != "agent_rate" || a[0].step != "D9" {
		t.Errorf("audit = %+v, want agent_rate at D9", a)
	}
}
