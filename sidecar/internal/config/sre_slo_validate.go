package config

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// check is one validation rule: ok, or the problem (never a secret).
type check struct {
	ok      bool
	problem string
}

func firstProblem(prefix string, checks []check) error {
	for _, c := range checks {
		if !c.ok {
			return fmt.Errorf("%s%s", prefix, c.problem)
		}
	}
	return nil
}

func (s SRESLOConfig) validate() error {
	p, pr := s.Prometheus, s.Proxies
	err := firstProblem("sre.slo.", []check{
		{s.EvaluationIntervalSeconds >= 15 && s.EvaluationIntervalSeconds <= 600,
			fmt.Sprintf("evaluation_interval_seconds must be 15-600, got %d",
				s.EvaluationIntervalSeconds)},
		{p.URL == "" || validPromURL(p.URL), "prometheus.url must be http(s)://host[:port] " +
			"without credentials (use bearer_token)"},
		{p.TimeoutSeconds >= 1 && p.TimeoutSeconds <= 60, fmt.Sprintf(
			"prometheus.timeout_seconds must be 1-60, got %d", p.TimeoutSeconds)},
		{p.BearerToken == "" || p.BearerTokenFile == "",
			"prometheus.bearer_token and prometheus.bearer_token_file are exclusive"},
		{secretOK(s.Push.HMACSecret), fmt.Sprintf(
			"push.hmac_secret must be at least %d bytes", minSecretBytes)},
		{toleranceOK(s.Push.TimestampToleranceSeconds),
			"push.timestamp_tolerance_seconds must be 30-3600"},
		{pr.Target > 0 && pr.Target <= 0.9999, "proxies.target must be in (0, 0.9999]"},
		{pr.WindowDays >= 1 && pr.WindowDays <= 90, "proxies.window_days must be 1-90"},
		{pr.LatencyThresholdMs >= 0 && !math.IsInf(pr.LatencyThresholdMs, 0),
			"proxies.latency_threshold_ms must not be negative"},
		{pr.LatencyFactor >= 1 && !math.IsInf(pr.LatencyFactor, 0),
			"proxies.latency_factor must be at least 1"},
		{pr.LatencyFloorMs >= 0, "proxies.latency_floor_ms must not be negative"},
		{pr.TopQueries >= 1 && pr.TopQueries <= 100, "proxies.top_queries must be 1-100"},
		{pr.ReplicationLagBudgetSeconds >= 1 && pr.ReplicationLagBudgetSeconds <= 86400,
			"proxies.replication_lag_budget_seconds must be 1-86400"},
	})
	if err != nil {
		return err
	}
	if err := s.validateRules(); err != nil {
		return err
	}
	return s.validateObjectives()
}

func toleranceOK(secs int) bool { return secs >= 30 && secs <= 3600 }

func (s SRESLOConfig) validateRules() error {
	rules, err := s.Rules()
	if err != nil {
		return err
	}
	for i, r := range rules {
		err := firstProblem(fmt.Sprintf("sre.slo.burn_rules[%d].", i), []check{
			{r.Severity == "page" || r.Severity == "ticket", "severity must be page or ticket"},
			{finitePositive(r.Factor), "factor must be positive"},
			{r.Short < r.Long, "short_window must be shorter than long_window"},
			{r.Long <= 90*24*time.Hour, "long_window must be at most 90d"},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

var objectiveName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

func (s SRESLOConfig) validateObjectives() error {
	seen := map[string]bool{}
	for i, o := range s.Objectives {
		prefix := fmt.Sprintf("sre.slo.objectives[%d] (%s).", i, o.Name)
		err := firstProblem(prefix, []check{
			{objectiveName.MatchString(o.Name), "name must match " + objectiveName.String()},
			{!strings.HasPrefix(o.Name, "db_"), "names starting with db_ are reserved " +
				"for the database proxies"},
			{!seen[o.Name], "name is duplicated"},
			{o.Target > 0 && o.Target < 1, "target must be in (0, 1)"},
			{o.WindowDays >= 0 && o.WindowDays <= 90, "window_days must be 1-90"},
			{o.MinEligibleEvents >= 0 && !math.IsInf(o.MinEligibleEvents, 0),
				"min_eligible_events must not be negative"},
			{o.StaleAfterSeconds >= 0, "stale_after_seconds must not be negative"},
		})
		if err != nil {
			return err
		}
		seen[o.Name] = true
		if err := s.validateSource(prefix, o); err != nil {
			return err
		}
	}
	return nil
}

func (s SRESLOConfig) validateSource(prefix string, o SREObjectiveConfig) error {
	hasWindow := func(q string) bool { return strings.Contains(q, "$window") }
	switch o.Source {
	case "prometheus":
		return firstProblem(prefix, []check{
			{s.Prometheus.URL != "", "a prometheus source needs sre.slo.prometheus.url"},
			{hasWindow(o.BadQuery) && hasWindow(o.EligibleQuery),
				"bad_query and eligible_query must use $window"},
			{o.ResetsQuery == "" || hasWindow(o.ResetsQuery), "resets_query must use $window"},
			{o.CoverageQuery == "" || hasWindow(o.CoverageQuery),
				"coverage_query must use $window"},
		})
	case "push":
		return firstProblem(prefix, []check{
			{s.Push.HMACSecret != "" || DatabricksAppMode(), "a push source needs sre.slo.push.hmac_secret outside Databricks Apps"},
			{!DatabricksAppMode() || o.Database != "", "a Databricks App push source needs an explicit database"},
			{o.BadQuery == "" && o.EligibleQuery == "" && o.ResetsQuery == "" &&
				o.CoverageQuery == "", "a push source takes no queries"},
		})
	}
	return fmt.Errorf("%ssource must be prometheus or push, got %q", prefix, o.Source)
}

var sourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

func (c SREChangeEventsConfig) validate() error {
	checks := []check{
		{secretOK(c.HMACSecret), fmt.Sprintf("hmac_secret must be at least %d bytes",
			minSecretBytes)},
		{toleranceOK(c.TimestampToleranceSeconds),
			"timestamp_tolerance_seconds must be 30-3600"},
		{c.FeedIntervalSeconds >= 15 && c.FeedIntervalSeconds <= 3600,
			"feed_interval_seconds must be 15-3600"},
		{c.RetentionDays >= 1 && c.RetentionDays <= maxSRERetentionDays,
			fmt.Sprintf("retention_days must be 1-%d", maxSRERetentionDays)},
	}
	for _, src := range c.AllowedSources {
		checks = append(checks, check{sourceName.MatchString(src),
			"allowed_sources entries must match " + sourceName.String()})
	}
	return firstProblem("sre.change_events.", checks)
}

// validateSLOOwners checks owner_database (playbooks-08). In a fleet or a
// meta-db fleet every database evaluates an objective with no database,
// so without an owner one burn would open an slo_burn investigation per
// database. Static fleets name their databases, so the owner must be one
// of them; meta-db fleets discover theirs (the sidecar warns at startup).
func (c *Config) validateSLOOwners() error {
	fleet := c.Mode == "fleet" || c.HasMetaDB()
	configured := make(map[string]bool, len(c.Databases))
	for _, db := range c.Databases {
		configured[db.Name] = true
	}
	for i, o := range c.SRE.SLO.Objectives {
		prefix := fmt.Sprintf("sre.slo.objectives[%d] (%s): ", i, o.Name)
		switch {
		case o.OwnerDatabase != "" && o.Database != "":
			return fmt.Errorf("%sowner_database applies only to an objective with no "+
				"database; remove it (database %q already owns this SLO)", prefix, o.Database)
		case o.OwnerDatabase != "" && !fleet:
			return fmt.Errorf("%sowner_database applies only in fleet and meta-db "+
				"modes; remove it (standalone monitors one database)", prefix)
		case o.OwnerDatabase == "" && o.Database == "" && fleet:
			return fmt.Errorf("%sin fleet mode an objective with no database is "+
				"evaluated by every database and would open one slo_burn investigation "+
				"per database; set database to the database the service uses, or set "+
				"owner_database to the database whose investigator handles its burns", prefix)
		case o.OwnerDatabase != "" && !c.HasMetaDB() && !configured[o.OwnerDatabase]:
			return fmt.Errorf("%sowner_database %q is not a configured database "+
				"(databases[].name)", prefix, o.OwnerDatabase)
		}
	}
	return nil
}
