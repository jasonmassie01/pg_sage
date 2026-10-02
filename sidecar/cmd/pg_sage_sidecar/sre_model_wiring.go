package main

import (
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
)

// sreModelClient is the LLM the investigator's model turn may consult:
// the database's general client when sre.llm.enabled (the default) and
// an LLM is configured. Without one, investigations stay deterministic
// and the process says why once.
func sreModelClient(settings config.SREConfig, client *llm.Client, notices *sre.OnceLog,
	logFn func(string, string, ...any)) *llm.Client {
	if !settings.LLM.Enabled {
		return nil
	}
	if client == nil || !client.IsEnabled() {
		sre.NoteModelUnavailable(notices, logFn, "no LLM is configured: set "+
			"llm.enabled, llm.endpoint and llm.api_key to use it, or set "+
			"sre.llm.enabled: false to keep investigations deterministic without this notice")
		return nil
	}
	return client
}

// sreLimits are the R1 investigation ceilings. With a model, the daily
// model allocation of each database and of the deployment is
// llm.token_budget_daily; without one it stays zero (no model use).
func sreLimits(model *llm.Client, dailyTokens int, notices *sre.OnceLog,
	logFn func(string, string, ...any)) sre.Limits {
	limits := sre.DefaultLimits()
	if model == nil {
		return limits
	}
	if dailyTokens <= 0 {
		notices.Log(logFn, sre.NoticeNoModelBudget, "WARN", "sre: llm.token_budget_daily "+
			"is 0, so the model turn has no daily allocation; investigations run "+
			"deterministically")
		return limits
	}
	limits.DatabaseDailyTokens = int64(dailyTokens)
	limits.DeploymentDailyTokens = int64(dailyTokens)
	return limits
}
