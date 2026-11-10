package main

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// sloObjectives are the registered app SLIs of one database: those
// naming it and those naming no database (every database's service).
func sloObjectives(cfg config.SRESLOConfig, database string) ([]slo.Objective, error) {
	var out []slo.Objective
	for _, o := range cfg.Objectives {
		if o.Database != "" && o.Database != database {
			continue
		}
		obj := slo.Objective{Name: o.Name, Database: database, OwnerDatabase: o.OwnerDatabase,
			Description: o.Description, Owner: o.Owner, Kind: slo.KindApp,
			Source: slo.SourceKind(o.Source), Target: o.Target, Window: o.Window(),
			BadQuery: o.BadQuery, EligibleQuery: o.EligibleQuery, ResetsQuery: o.ResetsQuery,
			CoverageQuery: o.CoverageQuery, MinEligible: o.MinEligible(),
			StaleAfter: o.StaleAfter()}
		if err := obj.Validate(); err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

// sloRules are the configured burn-rate rules, or the workbook defaults.
func sloRules(cfg config.SRESLOConfig) ([]slo.Rule, error) {
	parsed, err := cfg.Rules()
	if err != nil || len(parsed) == 0 {
		return slo.DefaultRules(), err
	}
	out := make([]slo.Rule, 0, len(parsed))
	for _, r := range parsed {
		out = append(out, slo.Rule{Severity: slo.Severity(r.Severity), Long: r.Long,
			Short: r.Short, Factor: r.Factor})
	}
	return out, slo.ValidateRules(out)
}

// sloProxyConfig tunes the database proxy SLIs.
func sloProxyConfig(p config.SREProxyConfig) slo.ProxyConfig {
	pc := slo.DefaultProxyConfig()
	pc.Target = p.Target
	pc.Window = time.Duration(p.WindowDays) * 24 * time.Hour
	pc.LatencyThresholdMs = p.LatencyThresholdMs
	pc.LatencyFactor = p.LatencyFactor
	pc.LatencyFloorMs = p.LatencyFloorMs
	pc.TopQueries = p.TopQueries
	pc.ReplicationLagBudget = time.Duration(p.ReplicationLagBudgetSeconds) * time.Second
	return pc
}

// prometheusClient is the SLI connector, or nil when no URL is set.
func prometheusClient(p config.SREPrometheusConfig) (*slo.PromClient, error) {
	if p.URL == "" {
		return nil, nil
	}
	token, err := p.Token()
	if err != nil {
		return nil, err
	}
	return slo.NewPromClient(slo.PromConfig{URL: p.URL, BearerToken: token,
		Timeout: p.Timeout()})
}

// missingSLOOwners describes each objective whose owner_database is not
// among the databases: its burns open no investigation anywhere. A
// meta-db fleet discovers its databases, so config validation cannot
// catch this; the sidecar warns at startup instead.
func missingSLOOwners(cfg config.SRESLOConfig, databases []string) []string {
	known := make(map[string]bool, len(databases))
	for _, name := range databases {
		known[name] = true
	}
	var out []string
	for _, o := range cfg.Objectives {
		if o.OwnerDatabase != "" && !known[o.OwnerDatabase] {
			out = append(out, fmt.Sprintf("sre.slo.objectives (%s): owner_database %q "+
				"is not a monitored database; its page-level burns open no slo_burn "+
				"investigation until a database of that name is registered",
				o.Name, o.OwnerDatabase))
		}
	}
	return out
}

// warnMissingSLOOwners logs missingSLOOwners at WARN.
func warnMissingSLOOwners(cfg config.SRESLOConfig, databases []string,
	logFn func(string, string, ...any)) {
	for _, msg := range missingSLOOwners(cfg, databases) {
		logFn("WARN", "sre slo: %s", msg)
	}
}

// sloOwners maps each objective with an owner_database to that owner.
func sloOwners(objs []slo.Objective) map[string]string {
	out := map[string]string{}
	for _, o := range objs {
		if o.OwnerDatabase != "" {
			out[o.Name] = o.OwnerDatabase
		}
	}
	return out
}

// unevaluatedOwnedSLOs names each objective that names database as its
// owner_database but that database's engine does not evaluate: a read
// policy that builds no engine, or keeps only bound push objectives. The
// other databases defer its burns to this one, so they would open no
// investigation anywhere. owners is the engine's sloOwners (nil: no engine).
func unevaluatedOwnedSLOs(cfg config.SRESLOConfig, database string,
	owners map[string]string) []string {
	var out []string
	for _, o := range cfg.Objectives {
		if o.OwnerDatabase == database && owners[o.Name] != database {
			out = append(out, o.Name)
		}
	}
	return out
}
