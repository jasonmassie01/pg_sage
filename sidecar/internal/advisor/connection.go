package advisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const connectionSystemPrompt = `You are a PostgreSQL connection management expert.

CRITICAL: Respond with ONLY a JSON array. No thinking, no reasoning outside JSON.

RULES:
1. max_connections caps the number of OPEN connections (active AND idle),
   not just active ones. Size every recommendation against TOTAL backends.
   NEVER size against active-only counts.
2. Only recommend reducing max_connections if it exceeds ~4x the peak TOTAL
   backends over the window AND there is comfortable headroom; reduce
   conservatively.
3. NEVER recommend max_connections below "Lowest safe max_connections"
   (the window's peak total backends + reserved slots + 10). If it is
   unknown, do not recommend lowering max_connections at all.
4. If sessions opened per minute > 10, recommend a connection pooler
   (PgBouncer) as a row with "kind":"pooler" and "recommended_sql":null.
5. If idle in transaction sessions average > 60s, recommend
   idle_in_transaction_session_timeout (between 1min and 24h, never 0) as
   ALTER SYSTEM SET; pg_sage applies it to this database only.
6. If a pooler is detected (from application_name), don't recommend adding one.
7. statement_timeout and idle_session_timeout are advice only: give them as
   "kind":"observation" rows with "recommended_sql":null.
8. If everything is healthy (utilization < 50%, no long idle tx), return [].

Each element: {"object_identifier":"instance","severity":"info",` +
	`"kind":"setting|pooler|observation",` +
	`"rationale":"...","recommended_sql":"ALTER SYSTEM SET ... or null",` +
	`"current_settings":{...},"recommended_settings":{...},` +
	`"peak_total_backends":N,"sessions_per_min":N}`

// The prompt's unknown facts (advisors-07).
const (
	churnUnknown = "unknown (the sessions counter does not cover the window, or the " +
		"statistics were reset)"
	floorUnknown = "unknown (the peak is unknown: do not recommend lowering " +
		"max_connections)"
)

// connectionSettings are the settings the connection prompt shows.
var connectionSettings = map[string]bool{
	"max_connections": true, "superuser_reserved_connections": true,
	"reserved_connections": true, "idle_in_transaction_session_timeout": true,
	"statement_timeout": true, "tcp_keepalives_idle": true,
	"tcp_keepalives_interval": true,
}

func analyzeConnections(
	ctx context.Context,
	mgr *llm.Manager,
	snap *collector.Snapshot,
	cfg *config.Config,
	logFn func(string, string, ...any),
) ([]analyzer.Finding, error) {
	if snap.ConfigData == nil {
		return nil, nil
	}
	env := connEnvFrom(ctx)
	if env.hist.window <= 0 {
		env.hist.window = cfg.Advisor.Interval()
	}
	fl := connectionFloor(snap.System, snap.ConfigData.PGSettings,
		promptEnvFrom(ctx).pgMajor, env.hist)
	prompt := buildConnectionPrompt(snap, openGateFrom(ctx), env, fl)
	grounding := promptGrounding(ctx, "max_connections",
		"idle_in_transaction_session_timeout")
	resp, err := chatAdvisor(ctx, mgr, "connection", connectionSystemPrompt, grounding,
		prompt)
	if err != nil {
		return nil, err
	}
	findings := parseLLMFindings(resp, "connection_tuning", logFn)
	findings = requireDatabaseOwner(findings, snap.ConfigData.OwnsDatabase, env.database)
	return filterUnsafeMaxConnections(findings, fl, logFn), nil
}

// buildConnectionPrompt states the facts with their scope: the connection
// states and sessions opened are this database's, the backends and slots
// the instance's (every database's).
func buildConnectionPrompt(snap *collector.Snapshot, gate openGate, env connEnv,
	fl connFloor) string {
	var settings []string
	for _, s := range snap.ConfigData.PGSettings {
		if connectionSettings[s.Name] && !gate.hasGUC("connection_tuning", s.Name) {
			settings = append(settings, fmt.Sprintf("  %s = %s%s", s.Name, s.Setting, s.Unit))
		}
	}
	var states []string
	for _, cs := range snap.ConfigData.ConnectionStates {
		states = append(states, fmt.Sprintf("    %s: count=%d, avg_duration=%.0fs",
			cs.State, cs.Count, cs.AvgDurationSeconds))
	}
	sys := snap.System
	return fmt.Sprintf("CONNECTION CONTEXT:\n\nSettings:\n%s\n\n"+
		"Advisor window: %s\n\n"+
		"%s:\n  Active backends: %d\n  Connection states:\n%s\n"+
		"  Sessions opened in the window: %s\n\n"+
		"Instance (all databases):\n  Total backends: %d\n"+
		"  Peak total backends in the window: %s\n  Max connections: %d\n"+
		"  Reserved slots: %d\n  Lowest safe max_connections: %s%s",
		strings.Join(settings, "\n"), env.hist.window, databaseLabel(env.database),
		sys.ActiveBackends, strings.Join(states, "\n"), churnFact(snap, env.hist),
		sys.TotalBackends, peakFact(fl), sys.MaxConnections, fl.reserved, floorFact(fl),
		gucNote(gate, "connection_tuning"))
}

func databaseLabel(database string) string {
	if strings.TrimSpace(database) == "" {
		return "This database"
	}
	return "This database (" + database + ")"
}

func churnFact(snap *collector.Snapshot, w connWindow) string {
	opened, perMin, ok := connChurn(snap, w)
	if !ok {
		return churnUnknown
	}
	return fmt.Sprintf("%d (%.1f/min)", opened, perMin)
}

func peakFact(fl connFloor) string {
	if !fl.known {
		return "unknown"
	}
	return fmt.Sprint(fl.peak)
}

func floorFact(fl connFloor) string {
	if !fl.known {
		return floorUnknown
	}
	return fmt.Sprintf("%d (peak or current total backends + reserved slots + %d)",
		fl.value, connFloorMargin)
}

// requireDatabaseOwner keeps a database-scope setting executable only when
// pg_sage's role owns the database: ALTER DATABASE ... SET needs it.
// Otherwise the advice stays, without SQL, for the owner to apply.
func requireDatabaseOwner(findings []analyzer.Finding, owns bool,
	database string) []analyzer.Finding {
	if owns {
		return findings
	}
	if strings.TrimSpace(database) == "" {
		database = "this database"
	}
	for i, f := range findings {
		stmt, ok := pgconf.ParseAlterSystem(f.RecommendedSQL)
		if ok && pgconf.DatabaseScopeApprovalOnly(stmt.Name) {
			findings[i] = advisoryOnly(f, fmt.Sprintf("pg_sage's role does not own %s, "+
				"so it cannot run ALTER DATABASE ... SET %s; the database owner can "+
				"apply it", database, stmt.Name))
		}
	}
	return findings
}
