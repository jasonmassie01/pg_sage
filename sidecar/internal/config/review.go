package config

import "fmt"

// Change Review: pg_sage reviews migration and data-access pull requests
// against the live database before they merge, and stamps a receipt
// after they land. On by default (LLM features are the product); without
// an LLM the deterministic review still runs. It reads; nothing it does
// executes DDL on a monitored database.
const (
	DefaultChangeReviewMaxTokensPerReview       = 60000
	DefaultChangeReviewDailyTokensPerDatabase   = 500000
	DefaultChangeReviewDailyTokensPerActor      = 200000
	DefaultChangeReviewReceiptWindowHours       = 24
	DefaultChangeReviewLandingExpiryDays        = 30
	DefaultChangeReviewMaxSQLBytes              = 100000
	DefaultChangeReviewMaxDiffBytes             = 524288
	DefaultChangeReviewMaxOfferedDiffBytes      = 200000
	DefaultChangeReviewMaxConcurrentPerDatabase = 2
	DefaultChangeReviewLockQueueWarn            = 100
	DefaultChangeReviewLockQueueCritical        = 5000
	DefaultChangeReviewBackfillRowsWarn         = 1_000_000
	DefaultChangeReviewBackfillRowsCritical     = 10_000_000
	DefaultChangeReviewLargeScanRows            = 100000
	DefaultChangeReviewAutoClearMaxTableBytes   = 10 << 30

	minReviewTokensPerReview = 4000
	maxReviewTokensPerReview = 500000
	maxReviewDailyTokens     = 100_000_000
	minReviewReceiptHours    = 2
	maxReviewReceiptHours    = 168
	maxReviewLandingDays     = 365
	// The SQL parser refuses scripts over 100,000 bytes before parsing.
	maxReviewSQLBytes       = 100000
	minReviewBytes          = 1024
	maxReviewDiffBytes      = 10 << 20
	maxReviewConcurrent     = 8
	maxReviewAutoClearBytes = 1 << 40
)

// ChangeReviewConfig configures Change Review. All fields are read when a
// database's runtime starts (restart lifecycle).
type ChangeReviewConfig struct {
	Enabled                  bool  `yaml:"enabled" doc:"Review migration and data-access changes against the live database (MCP review_change, REST, CLI). It reads, plans and runs session-local HypoPG what-ifs; it never runs the change. Default: true."`
	MaxTokensPerReview       int   `yaml:"max_tokens_per_review" doc:"Most LLM tokens one review may use across its model calls. Range 4000-500000. Default: 60000."`
	DailyTokensPerDatabase   int   `yaml:"daily_tokens_per_database" doc:"Change Review's own daily LLM tokens for one database, all callers (UTC day, kept across restarts). 0 runs deterministic reviews only. Range 0-100000000. Default: 500000."`
	DailyTokensPerActor      int   `yaml:"daily_tokens_per_actor" doc:"Change Review's daily LLM tokens for one caller (user or MCP token) on one database; at most daily_tokens_per_database. Range 0-100000000. Default: 200000."`
	ReceiptWindowHours       int   `yaml:"receipt_window_hours" doc:"Hours of workload compared before and after a reviewed change lands to decide its receipt. Range 2-168. Default: 24."`
	LandingExpiryDays        int   `yaml:"landing_expiry_days" doc:"Days a completed review waits to land before it expires (receipt unverifiable, credit void). Range 1-365. Default: 30."`
	MaxSQLBytes              int   `yaml:"max_sql_bytes" doc:"Largest migration SQL a review accepts, all files together. Range 1024-100000. Default: 100000."`
	MaxDiffBytes             int   `yaml:"max_diff_bytes" doc:"Largest unified diff a review accepts. The raw diff is never stored. Range 1024-10485760. Default: 524288."`
	MaxOfferedDiffBytes      int   `yaml:"max_offered_diff_bytes" doc:"Most redacted data-access diff bytes offered to the model; at most max_diff_bytes. Range 1024-10485760. Default: 200000."`
	MaxConcurrentPerDatabase int   `yaml:"max_concurrent_per_database" doc:"Reviews running at once on one database; 8 more may queue. Range 1-8. Default: 2."`
	LockQueueWarn            int   `yaml:"lock_queue_warn" doc:"Statements expected to queue behind a change's lock at peak rate that make it a warning (CR-02). At least 1. Default: 100."`
	LockQueueCritical        int   `yaml:"lock_queue_critical" doc:"Statements expected to queue behind a change's lock at peak rate that make it critical (CR-02); above lock_queue_warn. Default: 5000."`
	BackfillRowsWarn         int64 `yaml:"backfill_rows_warn" doc:"Rows a migration's UPDATE or DELETE touches that make it a warning (CR-12). At least 1. Default: 1000000."`
	BackfillRowsCritical     int64 `yaml:"backfill_rows_critical" doc:"Rows a migration's UPDATE or DELETE touches that make it critical (CR-12); above backfill_rows_warn. Default: 10000000."`
	LargeScanRows            int64 `yaml:"large_scan_rows" doc:"Estimated rows of a sequential scan in a reviewed statement's plan that are reported (CR-14). At least 1. Default: 100000."`
	AutoClearMaxTableBytes   int64 `yaml:"auto_clear_max_table_bytes" doc:"Largest table a CREATE INDEX CONCURRENTLY may target to count as shadow auto-clear eligible. Shadow only: nothing is cleared automatically. Range 0-1099511627776. Default: 10737418240 (10 GiB)."`
}

func defaultChangeReviewConfig() ChangeReviewConfig {
	return ChangeReviewConfig{Enabled: true,
		MaxTokensPerReview:       DefaultChangeReviewMaxTokensPerReview,
		DailyTokensPerDatabase:   DefaultChangeReviewDailyTokensPerDatabase,
		DailyTokensPerActor:      DefaultChangeReviewDailyTokensPerActor,
		ReceiptWindowHours:       DefaultChangeReviewReceiptWindowHours,
		LandingExpiryDays:        DefaultChangeReviewLandingExpiryDays,
		MaxSQLBytes:              DefaultChangeReviewMaxSQLBytes,
		MaxDiffBytes:             DefaultChangeReviewMaxDiffBytes,
		MaxOfferedDiffBytes:      DefaultChangeReviewMaxOfferedDiffBytes,
		MaxConcurrentPerDatabase: DefaultChangeReviewMaxConcurrentPerDatabase,
		LockQueueWarn:            DefaultChangeReviewLockQueueWarn,
		LockQueueCritical:        DefaultChangeReviewLockQueueCritical,
		BackfillRowsWarn:         DefaultChangeReviewBackfillRowsWarn,
		BackfillRowsCritical:     DefaultChangeReviewBackfillRowsCritical,
		LargeScanRows:            DefaultChangeReviewLargeScanRows,
		AutoClearMaxTableBytes:   DefaultChangeReviewAutoClearMaxTableBytes,
	}
}

// DefaultChangeReviewConfig is the configuration with every default set.
func DefaultChangeReviewConfig() ChangeReviewConfig { return defaultChangeReviewConfig() }

// Validate checks the ranges. Every error names its key. A disabled
// Change Review spends nothing, so its budgets are not compared.
func (c ChangeReviewConfig) Validate() error {
	if err := c.validateBudgets(); err != nil {
		return err
	}
	if err := c.validateLimits(); err != nil {
		return err
	}
	return c.validateThresholds()
}

func (c ChangeReviewConfig) validateBudgets() error {
	switch {
	case c.MaxTokensPerReview < minReviewTokensPerReview ||
		c.MaxTokensPerReview > maxReviewTokensPerReview:
		return reviewRange("max_tokens_per_review", minReviewTokensPerReview,
			maxReviewTokensPerReview, int64(c.MaxTokensPerReview))
	case c.DailyTokensPerDatabase < 0 || c.DailyTokensPerDatabase > maxReviewDailyTokens:
		return reviewRange("daily_tokens_per_database", 0, maxReviewDailyTokens,
			int64(c.DailyTokensPerDatabase))
	case c.DailyTokensPerActor < 0 || c.DailyTokensPerActor > maxReviewDailyTokens:
		return reviewRange("daily_tokens_per_actor", 0, maxReviewDailyTokens,
			int64(c.DailyTokensPerActor))
	case c.Enabled && c.DailyTokensPerActor > c.DailyTokensPerDatabase:
		return fmt.Errorf("change_review.daily_tokens_per_actor (%d) must not exceed "+
			"change_review.daily_tokens_per_database (%d)", c.DailyTokensPerActor,
			c.DailyTokensPerDatabase)
	}
	return nil
}

func (c ChangeReviewConfig) validateLimits() error {
	switch {
	case c.ReceiptWindowHours < minReviewReceiptHours ||
		c.ReceiptWindowHours > maxReviewReceiptHours:
		return reviewRange("receipt_window_hours", minReviewReceiptHours,
			maxReviewReceiptHours, int64(c.ReceiptWindowHours))
	case c.LandingExpiryDays < 1 || c.LandingExpiryDays > maxReviewLandingDays:
		return reviewRange("landing_expiry_days", 1, maxReviewLandingDays,
			int64(c.LandingExpiryDays))
	case c.MaxSQLBytes < minReviewBytes || c.MaxSQLBytes > maxReviewSQLBytes:
		return reviewRange("max_sql_bytes", minReviewBytes, maxReviewSQLBytes,
			int64(c.MaxSQLBytes))
	case c.MaxDiffBytes < minReviewBytes || c.MaxDiffBytes > maxReviewDiffBytes:
		return reviewRange("max_diff_bytes", minReviewBytes, maxReviewDiffBytes,
			int64(c.MaxDiffBytes))
	case c.MaxOfferedDiffBytes < minReviewBytes || c.MaxOfferedDiffBytes > c.MaxDiffBytes:
		return fmt.Errorf("change_review.max_offered_diff_bytes must be %d-%d "+
			"(change_review.max_diff_bytes), got %d", minReviewBytes, c.MaxDiffBytes,
			c.MaxOfferedDiffBytes)
	case c.MaxConcurrentPerDatabase < 1 || c.MaxConcurrentPerDatabase > maxReviewConcurrent:
		return reviewRange("max_concurrent_per_database", 1, maxReviewConcurrent,
			int64(c.MaxConcurrentPerDatabase))
	}
	return nil
}

func (c ChangeReviewConfig) validateThresholds() error {
	switch {
	case c.LockQueueWarn < 1:
		return fmt.Errorf("change_review.lock_queue_warn must be at least 1, got %d",
			c.LockQueueWarn)
	case c.LockQueueCritical <= c.LockQueueWarn:
		return fmt.Errorf("change_review.lock_queue_critical (%d) must exceed "+
			"change_review.lock_queue_warn (%d)", c.LockQueueCritical, c.LockQueueWarn)
	case c.BackfillRowsWarn < 1:
		return fmt.Errorf("change_review.backfill_rows_warn must be at least 1, got %d",
			c.BackfillRowsWarn)
	case c.BackfillRowsCritical <= c.BackfillRowsWarn:
		return fmt.Errorf("change_review.backfill_rows_critical (%d) must exceed "+
			"change_review.backfill_rows_warn (%d)", c.BackfillRowsCritical,
			c.BackfillRowsWarn)
	case c.LargeScanRows < 1:
		return fmt.Errorf("change_review.large_scan_rows must be at least 1, got %d",
			c.LargeScanRows)
	case c.AutoClearMaxTableBytes < 0 || c.AutoClearMaxTableBytes > maxReviewAutoClearBytes:
		return reviewRange("auto_clear_max_table_bytes", 0, maxReviewAutoClearBytes,
			c.AutoClearMaxTableBytes)
	}
	return nil
}

func reviewRange(key string, lo, hi, got int64) error {
	return fmt.Errorf("change_review.%s must be %d-%d, got %d", key, lo, hi, got)
}
