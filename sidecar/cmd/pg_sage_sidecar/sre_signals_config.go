package main

import (
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
		obj := slo.Objective{Name: o.Name, Database: database, Description: o.Description,
			Owner: o.Owner, Kind: slo.KindApp, Source: slo.SourceKind(o.Source),
			Target: o.Target, Window: o.Window(), BadQuery: o.BadQuery,
			EligibleQuery: o.EligibleQuery, ResetsQuery: o.ResetsQuery,
			MinEligible: o.MinEligible(), StaleAfter: o.StaleAfter()}
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
