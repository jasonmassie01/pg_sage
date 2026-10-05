package config

import (
	"strings"
	"testing"
)

// Ask Sage (roadmap phase 3): a conversational surface with its own LLM
// budget per database and per user. It is on by default (LLM features are
// the product), with a daily token allocation for the database, a smaller
// one per user, a per-question cap and a retention window for the stored
// conversations. An absent file and a partial section keep the defaults.

func TestAskConfig_DefaultsWithoutAFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defaults := map[string]AskConfig{"Load": cfg.Ask, "DefaultConfig": DefaultConfig().Ask}
	for name, got := range defaults {
		if !got.Enabled || got.DailyTokensPerDatabase != DefaultAskDailyTokensPerDatabase ||
			got.DailyTokensPerUser != DefaultAskDailyTokensPerUser ||
			got.MaxTokensPerQuestion != DefaultAskMaxTokensPerQuestion ||
			got.RetentionDays != DefaultAskRetentionDays {
			t.Errorf("%s ask defaults = %+v", name, got)
		}
	}
	if DefaultAskDailyTokensPerDatabase != 300000 || DefaultAskDailyTokensPerUser != 100000 ||
		DefaultAskMaxTokensPerQuestion != 40000 || DefaultAskRetentionDays != 30 {
		t.Fatalf("default constants changed: %d %d %d %d", DefaultAskDailyTokensPerDatabase,
			DefaultAskDailyTokensPerUser, DefaultAskMaxTokensPerQuestion, DefaultAskRetentionDays)
	}
}

func TestAskConfig_PartialSectionKeepsTheOtherDefaults(t *testing.T) {
	c, err := loadRCAYAML(t, "ask:\n  daily_tokens_per_user: 5000\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Ask.DailyTokensPerUser != 5000 || !c.Ask.Enabled ||
		c.Ask.DailyTokensPerDatabase != DefaultAskDailyTokensPerDatabase ||
		c.Ask.RetentionDays != DefaultAskRetentionDays {
		t.Fatalf("ask = %+v", c.Ask)
	}
	off, err := loadRCAYAML(t, "ask:\n  enabled: false\n")
	if err != nil || off.Ask.Enabled {
		t.Fatalf("enabled: false = %+v (%v)", off.Ask, err)
	}
}

func TestAskConfig_Boundaries(t *testing.T) {
	ok := []string{
		"ask:\n  daily_tokens_per_database: 0\n  daily_tokens_per_user: 0\n",
		"ask:\n  daily_tokens_per_database: 100000000\n  daily_tokens_per_user: 100000000\n",
		"ask:\n  max_tokens_per_question: 4000\n",
		"ask:\n  max_tokens_per_question: 200000\n",
		"ask:\n  retention_days: 1\n",
		"ask:\n  retention_days: 3650\n",
		"ask:\n  daily_tokens_per_database: 5000\n  daily_tokens_per_user: 5000\n",
	}
	for _, body := range ok {
		if _, err := loadRCAYAML(t, body); err != nil {
			t.Errorf("%q refused: %v", body, err)
		}
	}
	bad := map[string]string{
		"ask:\n  daily_tokens_per_database: -1\n":        "ask.daily_tokens_per_database",
		"ask:\n  daily_tokens_per_database: 100000001\n": "ask.daily_tokens_per_database",
		"ask:\n  daily_tokens_per_user: -5\n":            "ask.daily_tokens_per_user",
		"ask:\n  daily_tokens_per_database: 1000\n  daily_tokens_per_user: 1001\n": "" +
			"ask.daily_tokens_per_user",
		"ask:\n  max_tokens_per_question: 3999\n":   "ask.max_tokens_per_question",
		"ask:\n  max_tokens_per_question: 200001\n": "ask.max_tokens_per_question",
		"ask:\n  retention_days: 0\n":               "ask.retention_days",
		"ask:\n  retention_days: 3651\n":            "ask.retention_days",
	}
	for body, key := range bad {
		_, err := loadRCAYAML(t, body)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: error %v does not name %s", body, err, key)
		}
	}
}

func TestAskConfig_DisabledSkipsTheBudgetChecks(t *testing.T) {
	// A disabled Ask Sage never spends tokens, so its budgets are not
	// checked against each other (an operator may zero one first).
	if _, err := loadRCAYAML(t, "ask:\n  enabled: false\n  daily_tokens_per_database: 10\n"+
		"  daily_tokens_per_user: 20\n"); err != nil {
		t.Fatalf("disabled config refused: %v", err)
	}
}

func TestAskConfig_DocTagsNameTheDefaults(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	want := map[string]string{
		"ask.enabled":                   "Default: true",
		"ask.daily_tokens_per_database": "Default: 300000",
		"ask.daily_tokens_per_user":     "Default: 100000",
		"ask.max_tokens_per_question":   "Default: 40000",
		"ask.retention_days":            "Default: 30",
	}
	for key, fragment := range want {
		if !strings.Contains(docs[key], fragment) {
			t.Errorf("%s doc %q lacks %q", key, docs[key], fragment)
		}
	}
	for _, key := range []string{"ask.enabled", "ask.daily_tokens_per_database"} {
		if lc, ok := LookupFieldLifecycle(key); !ok || lc.Lifecycle != LifecycleRestart {
			t.Errorf("%s lifecycle = %+v (%v), want restart", key, lc, ok)
		}
	}
}
