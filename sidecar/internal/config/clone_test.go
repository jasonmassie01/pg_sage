package config

import "testing"

func TestCloneDetachesMutableCollections(t *testing.T) {
	original := &Config{
		Databases: []DatabaseConfig{{
			Name: "primary",
			Tags: []string{"prod"},
		}},
		Briefing: BriefingConfig{
			Channels: []string{"stdout"},
		},
		Alerting: AlertingConfig{
			Routes: []AlertRoute{{
				Severity: "critical",
				Channels: []string{"pagerduty"},
			}},
			Webhooks: []WebhookConfig{{
				Name: "ops",
				Headers: map[string]string{
					"Authorization": "secret",
				},
			}},
		},
		Forecaster: ForecasterConfig{
			AlertHorizons: []int{30, 7, 3},
		},
		Analyzer: AnalyzerConfig{
			LockChain: LockChainConfig{
				SafePatterns: []string{"pg_sage"},
			},
		},
		Runaway: RunawayConfig{
			Policies: []RunawayPolicy{{
				Name: "default",
			}},
			SafePatterns: []string{"pg_dump"},
		},
		LogWatch: LogWatchConfig{
			ExcludeApplications: []string{"pg_sage"},
		},
		SchemaLint: SchemaLintConfig{
			IncludeSchemas: []string{"public"},
			ExcludeSchemas: []string{"pg_catalog"},
			DisabledRules:  []string{"lint_serial_usage"},
		},
	}

	cloned := Clone(original)
	if cloned == nil {
		t.Fatal("Clone returned nil")
	}

	original.Databases[0].Tags[0] = "changed"
	original.Briefing.Channels[0] = "changed"
	original.Alerting.Routes[0].Channels[0] = "changed"
	original.Alerting.Webhooks[0].Headers["Authorization"] = "changed"
	original.Forecaster.AlertHorizons[0] = 1
	original.Analyzer.LockChain.SafePatterns[0] = "changed"
	original.Runaway.Policies[0].Name = "changed"
	original.Runaway.SafePatterns[0] = "changed"
	original.LogWatch.ExcludeApplications[0] = "changed"
	original.SchemaLint.IncludeSchemas[0] = "changed"
	original.SchemaLint.ExcludeSchemas[0] = "changed"
	original.SchemaLint.DisabledRules[0] = "changed"

	assertCloneEqual(t, cloned.Databases[0].Tags[0], "prod")
	assertCloneEqual(t, cloned.Briefing.Channels[0], "stdout")
	assertCloneEqual(t, cloned.Alerting.Routes[0].Channels[0], "pagerduty")
	assertCloneEqual(
		t, cloned.Alerting.Webhooks[0].Headers["Authorization"], "secret")
	if cloned.Forecaster.AlertHorizons[0] != 30 {
		t.Fatalf("AlertHorizons[0] = %d, want 30",
			cloned.Forecaster.AlertHorizons[0])
	}
	assertCloneEqual(t, cloned.Analyzer.LockChain.SafePatterns[0], "pg_sage")
	assertCloneEqual(t, cloned.Runaway.Policies[0].Name, "default")
	assertCloneEqual(t, cloned.Runaway.SafePatterns[0], "pg_dump")
	assertCloneEqual(t, cloned.LogWatch.ExcludeApplications[0], "pg_sage")
	assertCloneEqual(t, cloned.SchemaLint.IncludeSchemas[0], "public")
	assertCloneEqual(t, cloned.SchemaLint.ExcludeSchemas[0], "pg_catalog")
	assertCloneEqual(t, cloned.SchemaLint.DisabledRules[0], "lint_serial_usage")
}

func assertCloneEqual(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
