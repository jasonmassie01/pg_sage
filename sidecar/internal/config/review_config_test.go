package config

import (
	"strings"
	"testing"
)

// CHECK-FND-02: Change Review defaults with no configuration file, and
// validation errors that name the key.

func TestChangeReviewDefaultsWithoutConfigFile(t *testing.T) {
	c := DefaultConfig().ChangeReview
	want := ChangeReviewConfig{Enabled: true, MaxTokensPerReview: 60000,
		DailyTokensPerDatabase: 500000, DailyTokensPerActor: 200000, ReceiptWindowHours: 24,
		LandingExpiryDays: 30, MaxSQLBytes: 100000, MaxDiffBytes: 524288,
		MaxOfferedDiffBytes: 200000, MaxConcurrentPerDatabase: 2, LockQueueWarn: 100,
		LockQueueCritical: 5000, BackfillRowsWarn: 1000000, BackfillRowsCritical: 10000000,
		LargeScanRows: 100000, AutoClearMaxTableBytes: 10737418240}
	if c != want {
		t.Fatalf("defaults = %+v\nwant     %+v", c, want)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
}

func TestChangeReviewDefaultsSurviveAnEmptySection(t *testing.T) {
	cfg, err := loadYAMLForTest(t, "change_review: {}\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ChangeReview != DefaultChangeReviewConfig() {
		t.Fatalf("an empty section zeroed defaults: %+v", cfg.ChangeReview)
	}
	cfg, err = loadYAMLForTest(t, "change_review:\n  receipt_window_hours: 48\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ChangeReview.ReceiptWindowHours != 48 || cfg.ChangeReview.MaxSQLBytes != 100000 {
		t.Fatalf("one key changed the others: %+v", cfg.ChangeReview)
	}
}

func TestChangeReviewValidateNamesTheKey(t *testing.T) {
	cases := map[string]struct {
		edit func(*crc)
		key  string
	}{
		"receipt window 1":   {func(c *crc) { c.ReceiptWindowHours = 1 }, "receipt_window_hours"},
		"receipt window 169": {func(c *crc) { c.ReceiptWindowHours = 169 }, "receipt_window_hours"},
		"concurrency 0": {func(c *crc) { c.MaxConcurrentPerDatabase = 0 },
			"max_concurrent_per_database"},
		"concurrency 9": {func(c *crc) { c.MaxConcurrentPerDatabase = 9 },
			"max_concurrent_per_database"},
		"actor over database": {func(c *crc) {
			c.DailyTokensPerActor = c.DailyTokensPerDatabase + 1
		}, "daily_tokens_per_actor"},
		"tokens per review": {func(c *crc) { c.MaxTokensPerReview = 100 }, "max_tokens_per_review"},
		"negative daily": {func(c *crc) { c.DailyTokensPerDatabase = -1 },
			"daily_tokens_per_database"},
		"landing expiry 0":  {func(c *crc) { c.LandingExpiryDays = 0 }, "landing_expiry_days"},
		"sql bytes too big": {func(c *crc) { c.MaxSQLBytes = 100001 }, "max_sql_bytes"},
		"diff bytes small":  {func(c *crc) { c.MaxDiffBytes = 10 }, "max_diff_bytes"},
		"offered over diff": {func(c *crc) { c.MaxOfferedDiffBytes = c.MaxDiffBytes + 1 },
			"max_offered_diff_bytes"},
		"lock queue order": {func(c *crc) { c.LockQueueCritical = c.LockQueueWarn },
			"lock_queue_critical"},
		"lock queue warn 0": {func(c *crc) { c.LockQueueWarn = 0 }, "lock_queue_warn"},
		"backfill order":    {func(c *crc) { c.BackfillRowsCritical = 5 }, "backfill_rows_critical"},
		"backfill warn 0":   {func(c *crc) { c.BackfillRowsWarn = 0 }, "backfill_rows_warn"},
		"large scan 0":      {func(c *crc) { c.LargeScanRows = 0 }, "large_scan_rows"},
		"auto clear negative": {func(c *crc) { c.AutoClearMaxTableBytes = -1 },
			"auto_clear_max_table_bytes"},
	}
	for name, tc := range cases {
		c := DefaultChangeReviewConfig()
		tc.edit(&c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "change_review."+tc.key) {
			t.Errorf("%s: %q does not name change_review.%s", name, err, tc.key)
		}
	}
}

func TestChangeReviewValidateBoundaries(t *testing.T) {
	for name, edit := range map[string]func(*crc){
		"receipt window 2":      func(c *crc) { c.ReceiptWindowHours = 2 },
		"receipt window 168":    func(c *crc) { c.ReceiptWindowHours = 168 },
		"concurrency 1":         func(c *crc) { c.MaxConcurrentPerDatabase = 1 },
		"concurrency 8":         func(c *crc) { c.MaxConcurrentPerDatabase = 8 },
		"actor equals database": func(c *crc) { c.DailyTokensPerActor = c.DailyTokensPerDatabase },
		"zero daily budgets":    func(c *crc) { c.DailyTokensPerDatabase, c.DailyTokensPerActor = 0, 0 },
		"offered equals diff":   func(c *crc) { c.MaxOfferedDiffBytes = c.MaxDiffBytes },
		"auto clear zero":       func(c *crc) { c.AutoClearMaxTableBytes = 0 },
		"disabled actor over cap": func(c *crc) {
			c.Enabled = false
			c.DailyTokensPerActor = c.DailyTokensPerDatabase + 1
		},
	} {
		c := DefaultChangeReviewConfig()
		edit(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestChangeReviewValidatedByConfig(t *testing.T) {
	_, err := loadYAMLForTest(t, "change_review:\n  max_concurrent_per_database: 9\n")
	if err == nil || !strings.Contains(err.Error(), "change_review.max_concurrent_per_database") {
		t.Fatalf("load with concurrency 9 = %v, want a validation error naming the key", err)
	}
}

func TestChangeReviewKeysClassified(t *testing.T) {
	want := map[string]KeyClass{
		"change_review.enabled":                   KeySafetyCritical,
		"change_review.max_tokens_per_review":     KeyDerivable,
		"change_review.daily_tokens_per_database": KeyDerivable,
		"change_review.receipt_window_hours":      KeySafetyCritical,
	}
	for key, class := range want {
		if got, ok := KeyClassOf(key); !ok || got != class {
			t.Errorf("%s classified %q (%v), want %q", key, got, ok, class)
		}
	}
}

// loadYAMLForTest loads a standalone configuration with extra appended.
func loadYAMLForTest(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return loadYAMLResult(t, standaloneIOYAML+extra)
}

// crc keeps the validation tables within the line limit.
type crc = ChangeReviewConfig
