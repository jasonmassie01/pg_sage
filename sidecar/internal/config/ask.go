package config

import "fmt"

// Ask Sage (roadmap phase 3): a conversational surface on the agent loop
// with its own daily LLM budget per database and per user, separate from
// the investigator's and the tuning agent's. On by default (LLM features
// are the product); without an LLM it answers that one is needed.
const (
	DefaultAskDailyTokensPerDatabase = 300000
	DefaultAskDailyTokensPerUser     = 100000
	DefaultAskMaxTokensPerQuestion   = 40000
	DefaultAskRetentionDays          = 30
	DefaultAskMaxSecondsPerQuestion  = 60
	DefaultAskMaxConcurrentRuns      = 4

	maxAskDailyTokens        = 100_000_000
	minAskTokensPerQuestion  = 4000
	maxAskTokensPerQuestion  = 200000
	maxAskRetentionDays      = 3650
	minAskRetentionDays      = 1
	minAskSecondsPerQuestion = 15
	maxAskSecondsPerQuestion = 300
	minAskConcurrentRuns     = 1
	maxAskConcurrentRuns     = 16
)

// AskConfig configures Ask Sage. All fields are read when a database's
// runtime starts (restart lifecycle).
type AskConfig struct {
	Enabled                bool `yaml:"enabled" doc:"Answer questions about each database from cited evidence (UI, API, MCP ask_sage). It reads; it never executes or approves. Default: true."`
	DailyTokensPerDatabase int  `yaml:"daily_tokens_per_database" doc:"Ask Sage's own daily LLM tokens for one database, all users (UTC day, kept across restarts). 0 refuses questions. Range 0-100000000. Default: 300000."`
	DailyTokensPerUser     int  `yaml:"daily_tokens_per_user" doc:"Ask Sage's daily LLM tokens for one user or MCP token on one database; at most daily_tokens_per_database. Range 0-100000000. Default: 100000."`
	MaxTokensPerQuestion   int  `yaml:"max_tokens_per_question" doc:"Most LLM tokens one question may use across its model calls. Range 4000-200000. Default: 40000."`
	RetentionDays          int  `yaml:"retention_days" doc:"Days a conversation is kept after its last question, with its answers. Range 1-3650. Default: 30."`
	MaxSecondsPerQuestion  int  `yaml:"max_seconds_per_question" doc:"Wall clock of one question (each model call is still bounded at 15 s). The ask API route gets this plus 5 s; MCP calls keep their own deadline. Range 15-300. Default: 60."`
	MaxConcurrentRuns      int  `yaml:"max_concurrent_runs" doc:"Asynchronous Ask Sage runs (the dashboard's questions) at once per database on one sidecar; one more is refused with HTTP 429 and Retry-After. Range 1-16. Default: 4."`
}

func defaultAskConfig() AskConfig {
	return AskConfig{Enabled: true, DailyTokensPerDatabase: DefaultAskDailyTokensPerDatabase,
		DailyTokensPerUser:    DefaultAskDailyTokensPerUser,
		MaxTokensPerQuestion:  DefaultAskMaxTokensPerQuestion,
		RetentionDays:         DefaultAskRetentionDays,
		MaxSecondsPerQuestion: DefaultAskMaxSecondsPerQuestion,
		MaxConcurrentRuns:     DefaultAskMaxConcurrentRuns}
}

// validate checks the ranges; a disabled Ask Sage spends nothing, so its
// budgets are not compared with each other.
func (a AskConfig) validate() error {
	switch {
	case a.DailyTokensPerDatabase < 0 || a.DailyTokensPerDatabase > maxAskDailyTokens:
		return fmt.Errorf("ask.daily_tokens_per_database must be 0-%d, got %d",
			maxAskDailyTokens, a.DailyTokensPerDatabase)
	case a.DailyTokensPerUser < 0 || a.DailyTokensPerUser > maxAskDailyTokens:
		return fmt.Errorf("ask.daily_tokens_per_user must be 0-%d, got %d",
			maxAskDailyTokens, a.DailyTokensPerUser)
	case a.Enabled && a.DailyTokensPerUser > a.DailyTokensPerDatabase:
		return fmt.Errorf("ask.daily_tokens_per_user (%d) must not exceed "+
			"ask.daily_tokens_per_database (%d)", a.DailyTokensPerUser,
			a.DailyTokensPerDatabase)
	case a.MaxTokensPerQuestion < minAskTokensPerQuestion ||
		a.MaxTokensPerQuestion > maxAskTokensPerQuestion:
		return fmt.Errorf("ask.max_tokens_per_question must be %d-%d, got %d",
			minAskTokensPerQuestion, maxAskTokensPerQuestion, a.MaxTokensPerQuestion)
	case a.MaxSecondsPerQuestion < minAskSecondsPerQuestion ||
		a.MaxSecondsPerQuestion > maxAskSecondsPerQuestion:
		return fmt.Errorf("ask.max_seconds_per_question must be %d-%d, got %d",
			minAskSecondsPerQuestion, maxAskSecondsPerQuestion, a.MaxSecondsPerQuestion)
	case a.MaxConcurrentRuns < minAskConcurrentRuns || a.MaxConcurrentRuns > maxAskConcurrentRuns:
		return fmt.Errorf("ask.max_concurrent_runs must be %d-%d, got %d", minAskConcurrentRuns,
			maxAskConcurrentRuns, a.MaxConcurrentRuns)
	case a.RetentionDays < minAskRetentionDays || a.RetentionDays > maxAskRetentionDays:
		return fmt.Errorf("ask.retention_days must be %d-%d, got %d", minAskRetentionDays,
			maxAskRetentionDays, a.RetentionDays)
	}
	return nil
}
