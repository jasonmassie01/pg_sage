package config

// Clone returns a copy of cfg with slice fields detached from the
// original. It is intended for immutable baseline snapshots used by
// hot-reload/reset code.
func Clone(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	cp := *cfg
	cp.Databases = append([]DatabaseConfig(nil), cfg.Databases...)
	for i := range cp.Databases {
		cp.Databases[i].Tags = append(
			[]string(nil), cfg.Databases[i].Tags...)
	}
	cp.Briefing.Channels = append(
		[]string(nil), cfg.Briefing.Channels...)
	cp.Alerting.Routes = append(
		[]AlertRoute(nil), cfg.Alerting.Routes...)
	for i := range cp.Alerting.Routes {
		cp.Alerting.Routes[i].Channels = append(
			[]string(nil), cfg.Alerting.Routes[i].Channels...)
	}
	cp.Alerting.Webhooks = append(
		[]WebhookConfig(nil), cfg.Alerting.Webhooks...)
	for i := range cp.Alerting.Webhooks {
		if cfg.Alerting.Webhooks[i].Headers == nil {
			continue
		}
		cp.Alerting.Webhooks[i].Headers = make(
			map[string]string, len(cfg.Alerting.Webhooks[i].Headers))
		for k, v := range cfg.Alerting.Webhooks[i].Headers {
			cp.Alerting.Webhooks[i].Headers[k] = v
		}
	}
	cp.Forecaster.AlertHorizons = append(
		[]int(nil), cfg.Forecaster.AlertHorizons...)
	cp.Analyzer.LockChain.SafePatterns = append(
		[]string(nil), cfg.Analyzer.LockChain.SafePatterns...)
	cp.Runaway.Policies = append(
		[]RunawayPolicy(nil), cfg.Runaway.Policies...)
	cp.Runaway.SafePatterns = append(
		[]string(nil), cfg.Runaway.SafePatterns...)
	cp.LogWatch.ExcludeApplications = append(
		[]string(nil), cfg.LogWatch.ExcludeApplications...)
	cp.SchemaLint.IncludeSchemas = append(
		[]string(nil), cfg.SchemaLint.IncludeSchemas...)
	cp.SchemaLint.ExcludeSchemas = append(
		[]string(nil), cfg.SchemaLint.ExcludeSchemas...)
	cp.SchemaLint.DisabledRules = append(
		[]string(nil), cfg.SchemaLint.DisabledRules...)
	return &cp
}
