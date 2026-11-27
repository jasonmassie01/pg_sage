package firstlook

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// CapLogRCA is log-based root-cause analysis: pg_sage reading the server log.
const CapLogRCA = "log_rca"

// RuleLogRCA flags a server whose log pg_sage cannot read yet.
const RuleLogRCA = "log_rca_unavailable"

// LogSettings are the server settings log-based RCA depends on. Known is
// false when they could not be read.
type LogSettings struct {
	Known            bool
	ServerVersionNum int
	Destination      string
	LoggingCollector string
	LCMessages       string
}

const logSettingsSQL = tag + `SELECT current_setting('server_version_num')::int,
  coalesce(current_setting('log_destination', true), ''),
  coalesce(current_setting('logging_collector', true), ''),
  coalesce(current_setting('lc_messages', true), '')`

// readLogSettings reads the log settings in the first look's read-only
// transaction (current_setting only).
func readLogSettings(ctx context.Context, tx pgx.Tx) (LogSettings, error) {
	s := LogSettings{Known: true}
	if err := tx.QueryRow(ctx, logSettingsSQL).Scan(&s.ServerVersionNum, &s.Destination,
		&s.LoggingCollector, &s.LCMessages); err != nil {
		return LogSettings{}, fmt.Errorf("read log settings: %w", err)
	}
	return s, nil
}

// fileFormat is the log format the server writes to files, or "".
func (s LogSettings) fileFormat() string {
	for _, d := range strings.Split(s.Destination, ",") {
		if d = strings.TrimSpace(d); d == "jsonlog" || d == "csvlog" {
			return d
		}
	}
	return ""
}

// LogRCACapability reports whether pg_sage can read this server's log and
// the steps that make it readable on provider: server settings when
// self-managed, the provider's log sink when managed.
func LogRCACapability(s LogSettings, provider string) Capability {
	c := Capability{Name: CapLogRCA, Status: CapabilityMissing}
	if steps, detail, ok := managedLogSteps(provider); ok {
		c.Status, c.Detail, c.Steps = CapabilityUnavailable, detail, steps
		if provider == "supabase" || provider == "lakebase" {
			c.Status = CapabilityMissing
		}
		return c
	}
	if !s.Known {
		c.Detail = "pg_sage could not read the log settings; log-based RCA needs " +
			"csvlog or jsonlog files and logging_collector = on."
		c.Steps = selfManagedLogSteps(s, true)
		return c
	}
	format, collector := s.fileFormat(), s.LoggingCollector == "on"
	if format != "" && collector {
		c.Status = CapabilityOK
		c.Detail = "The server writes " + format + " files pg_sage can tail."
		if s.LCMessages != "" && logwatch.LCMessagesWarning(s.LCMessages) != "" {
			c.Detail += fmt.Sprintf(" lc_messages is %q: text-matched signals need C or "+
				"English messages (set lc_messages = 'C').", s.LCMessages)
		}
		return c
	}
	c.Detail = fmt.Sprintf("log_destination is %q and logging_collector is %q: "+
		"pg_sage reads only csvlog or jsonlog files written by the logging "+
		"collector.", s.Destination, s.LoggingCollector)
	c.Steps = selfManagedLogSteps(s, format == "")
	return c
}

// selfManagedLogSteps are the ALTER SYSTEM steps and the sidecar setup.
func selfManagedLogSteps(s LogSettings, needDestination bool) []string {
	var steps []string
	if needDestination {
		format := "jsonlog"
		if s.ServerVersionNum > 0 && s.ServerVersionNum < 150000 {
			format = "csvlog" // jsonlog arrived in PostgreSQL 15
		}
		steps = append(steps, "As a superuser: ALTER SYSTEM SET log_destination = "+
			"'stderr,"+format+"';")
	}
	if s.LoggingCollector == "on" {
		// The collector already runs: log_destination applies on a reload.
		steps = append(steps, "ALTER SYSTEM SET lc_messages = 'C';",
			"Then reload PostgreSQL: SELECT pg_reload_conf();")
	} else {
		steps = append(steps, "ALTER SYSTEM SET logging_collector = on; "+
			"ALTER SYSTEM SET lc_messages = 'C';",
			"Then restart PostgreSQL (logging_collector applies only at server start).")
	}
	steps = append(steps, "Mount the server's log_directory read-only into the sidecar; "+
		"with logwatch.enabled at auto (the default) or true and rca.enabled: true, "+
		"log RCA then starts by itself.")
	return steps
}

// managedLogSteps points a managed service to its own log sink.
func managedLogSteps(provider string) (steps []string, detail string, ok bool) {
	const noFiles = "The server's log files are not reachable on this service. "
	switch provider {
	case "rds", "aurora":
		return []string{"PostgreSQL logs go to the RDS log files and CloudWatch Logs " +
			"(export them with the instance's log exports); pg_sage does not read " +
			"CloudWatch, so log-based RCA is unavailable here."}, noFiles, true
	case "cloud-sql", "alloydb":
		return []string{"PostgreSQL logs go to Cloud Logging; pg_sage does not read " +
			"Cloud Logging, so log-based RCA is unavailable here."}, noFiles, true
	case "azure", "azure-cosmos":
		return []string{"PostgreSQL logs go to Azure Monitor (diagnostic settings); " +
			"pg_sage does not read Azure Monitor, so log-based RCA is unavailable " +
			"here."}, noFiles, true
	case "supabase":
		return []string{"Set SAGE_SUPABASE_OBSERVABILITY_TOKEN: pg_sage then reads the " +
			"logs through the Supabase Management API.",
			"Keep logwatch.enabled at auto (the default) or true, and rca.enabled: true.",
		}, noFiles, true
	case "lakebase":
		return []string{"Lakebase exposes PostgreSQL logs through system.lakebase: " +
			"configure the opt-in lakebase_observability reader and inspect its " +
			"source status."}, noFiles, true
	case "neon":
		return []string{"Neon does not expose PostgreSQL log files; log-based RCA is " +
			"unavailable here."}, noFiles, true
	}
	return nil, "", false
}

// LogRCAItem is the info item for a log source that is not ready, with the
// steps as its recommendation; ok is false for a ready or empty capability.
func LogRCAItem(c Capability) (Item, bool) {
	if c.Name != CapLogRCA || c.Status == CapabilityOK || c.Status == "" {
		return Item{}, false
	}
	return Item{Rule: RuleLogRCA, Severity: SeverityInfo, Object: CapLogRCA,
		Title:  "Log-based RCA cannot read this server's log",
		Detail: c.Detail, Recommendation: strings.Join(c.Steps, " "),
		Evidence: []Evidence{{Source: "pg_settings",
			Ref:    "log_destination, logging_collector, lc_messages",
			Detail: fmt.Sprintf("%s status %s", c.Name, c.Status)}}}, true
}
