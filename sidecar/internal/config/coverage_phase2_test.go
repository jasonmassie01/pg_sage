package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Interval helpers (lines 257-289) — all 0% coverage
// ---------------------------------------------------------------------------

func TestPhase2_CollectorInterval(t *testing.T) {
	c := &CollectorConfig{IntervalSeconds: 30}
	got := c.Interval()
	want := 30 * time.Second
	if got != want {
		t.Errorf("CollectorConfig.Interval() = %v, want %v", got, want)
	}
}

func TestPhase2_CollectorInterval_Zero(t *testing.T) {
	c := &CollectorConfig{IntervalSeconds: 0}
	got := c.Interval()
	if got != 0 {
		t.Errorf("CollectorConfig.Interval() with 0 = %v, want 0", got)
	}
}

func TestPhase2_AnalyzerInterval(t *testing.T) {
	c := &AnalyzerConfig{IntervalSeconds: 600}
	got := c.Interval()
	want := 600 * time.Second
	if got != want {
		t.Errorf("AnalyzerConfig.Interval() = %v, want %v", got, want)
	}
}

func TestPhase2_AnalyzerInterval_Zero(t *testing.T) {
	c := &AnalyzerConfig{IntervalSeconds: 0}
	got := c.Interval()
	if got != 0 {
		t.Errorf("AnalyzerConfig.Interval() with 0 = %v, want 0", got)
	}
}

func TestPhase2_AdvisorInterval(t *testing.T) {
	c := &AdvisorConfig{IntervalSeconds: 86400}
	got := c.Interval()
	want := 86400 * time.Second
	if got != want {
		t.Errorf("AdvisorConfig.Interval() = %v, want %v", got, want)
	}
}

func TestPhase2_AdvisorInterval_Zero(t *testing.T) {
	c := &AdvisorConfig{IntervalSeconds: 0}
	got := c.Interval()
	if got != 0 {
		t.Errorf("AdvisorConfig.Interval() with 0 = %v, want 0", got)
	}
}

func TestPhase2_SafetyDormantInterval(t *testing.T) {
	c := &SafetyConfig{DormantIntervalSeconds: 600}
	got := c.DormantInterval()
	want := 600 * time.Second
	if got != want {
		t.Errorf("SafetyConfig.DormantInterval() = %v, want %v", got, want)
	}
}

func TestPhase2_SafetyDormantInterval_Zero(t *testing.T) {
	c := &SafetyConfig{DormantIntervalSeconds: 0}
	got := c.DormantInterval()
	if got != 0 {
		t.Errorf("SafetyConfig.DormantInterval() with 0 = %v, want 0", got)
	}
}

func TestPhase2_AlertingCheckInterval(t *testing.T) {
	c := &AlertingConfig{CheckIntervalSeconds: 60}
	got := c.CheckInterval()
	want := 60 * time.Second
	if got != want {
		t.Errorf("AlertingConfig.CheckInterval() = %v, want %v", got, want)
	}
}

func TestPhase2_AlertingCheckInterval_Zero(t *testing.T) {
	c := &AlertingConfig{CheckIntervalSeconds: 0}
	got := c.CheckInterval()
	if got != 0 {
		t.Errorf("AlertingConfig.CheckInterval() with 0 = %v, want 0", got)
	}
}

func TestPhase2_AutoExplainCollectInterval(t *testing.T) {
	c := &AutoExplainConfig{CollectIntervalSeconds: 300}
	got := c.CollectInterval()
	want := 300 * time.Second
	if got != want {
		t.Errorf("AutoExplainConfig.CollectInterval() = %v, want %v",
			got, want)
	}
}

func TestPhase2_AutoExplainCollectInterval_Zero(t *testing.T) {
	c := &AutoExplainConfig{CollectIntervalSeconds: 0}
	got := c.CollectInterval()
	if got != 0 {
		t.Errorf("AutoExplainConfig.CollectInterval() with 0 = %v, want 0",
			got)
	}
}

func TestPhase2_SafetyQueryTimeout(t *testing.T) {
	c := &SafetyConfig{QueryTimeoutMs: 500}
	got := c.QueryTimeout()
	want := 500 * time.Millisecond
	if got != want {
		t.Errorf("SafetyConfig.QueryTimeout() = %v, want %v", got, want)
	}
}

func TestPhase2_SafetyQueryTimeout_Zero(t *testing.T) {
	c := &SafetyConfig{QueryTimeoutMs: 0}
	got := c.QueryTimeout()
	if got != 0 {
		t.Errorf("SafetyConfig.QueryTimeout() with 0 = %v, want 0", got)
	}
}

func TestPhase2_SafetyDDLTimeout(t *testing.T) {
	c := &SafetyConfig{DDLTimeoutSeconds: 300}
	got := c.DDLTimeout()
	want := 300 * time.Second
	if got != want {
		t.Errorf("SafetyConfig.DDLTimeout() = %v, want %v", got, want)
	}
}

func TestPhase2_SafetyDDLTimeout_Zero(t *testing.T) {
	c := &SafetyConfig{DDLTimeoutSeconds: 0}
	got := c.DDLTimeout()
	if got != 0 {
		t.Errorf("SafetyConfig.DDLTimeout() with 0 = %v, want 0", got)
	}
}

func TestPhase2_SafetyLockTimeout_Default(t *testing.T) {
	c := &SafetyConfig{LockTimeoutMs: 0}
	got := c.LockTimeout()
	if got != DefaultLockTimeoutMs {
		t.Errorf("LockTimeout() with 0 = %d, want default %d",
			got, DefaultLockTimeoutMs)
	}
}

func TestPhase2_SafetyLockTimeout_Negative(t *testing.T) {
	c := &SafetyConfig{LockTimeoutMs: -1}
	got := c.LockTimeout()
	if got != DefaultLockTimeoutMs {
		t.Errorf("LockTimeout() with -1 = %d, want default %d",
			got, DefaultLockTimeoutMs)
	}
}

func TestPhase2_SafetyLockTimeout_Positive(t *testing.T) {
	c := &SafetyConfig{LockTimeoutMs: 5000}
	got := c.LockTimeout()
	if got != 5000 {
		t.Errorf("LockTimeout() = %d, want 5000", got)
	}
}

// ---------------------------------------------------------------------------
// warnUnexpandedEnvVars (22.2% coverage)
// ---------------------------------------------------------------------------

func TestPhase2_WarnUnexpandedEnvVars_NoVars(t *testing.T) {
	// No ${} references — should not panic or error.
	warnUnexpandedEnvVars("key: value", "key: value")
}

func TestPhase2_WarnUnexpandedEnvVars_SetVar(t *testing.T) {
	t.Setenv("SAGE_TEST_WARN_VAR", "hello")
	raw := "api_key: ${SAGE_TEST_WARN_VAR}"
	expanded := os.ExpandEnv(raw)
	// Should not warn because the env var IS set.
	warnUnexpandedEnvVars(raw, expanded)
}

func TestPhase2_WarnUnexpandedEnvVars_UnsetVar(t *testing.T) {
	// Ensure this var is NOT set.
	os.Unsetenv("SAGE_TEST_UNSET_VAR_12345")
	raw := "api_key: ${SAGE_TEST_UNSET_VAR_12345}"
	expanded := os.ExpandEnv(raw)
	// Should warn (writes to stderr). We just verify no panic.
	warnUnexpandedEnvVars(raw, expanded)
}

func TestPhase2_WarnUnexpandedEnvVars_MultipleVars(t *testing.T) {
	t.Setenv("SAGE_TEST_SET_A", "aaa")
	os.Unsetenv("SAGE_TEST_UNSET_B")
	raw := "a: ${SAGE_TEST_SET_A}\nb: ${SAGE_TEST_UNSET_B}"
	expanded := os.ExpandEnv(raw)
	warnUnexpandedEnvVars(raw, expanded)
}

func TestPhase2_WarnUnexpandedEnvVars_UnclosedBrace(t *testing.T) {
	// Unclosed ${... should not panic.
	raw := "api_key: ${SAGE_UNCLOSED"
	expanded := os.ExpandEnv(raw)
	warnUnexpandedEnvVars(raw, expanded)
}

func TestPhase2_WarnUnexpandedEnvVars_EmptyVarName(t *testing.T) {
	raw := "api_key: ${}"
	expanded := os.ExpandEnv(raw)
	warnUnexpandedEnvVars(raw, expanded)
}

// ---------------------------------------------------------------------------
// envOr (66.7% coverage — need to hit the default branch)
// ---------------------------------------------------------------------------

func TestPhase2_EnvOr_Set(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVOR_SET", "found")
	got := envOr("SAGE_TEST_ENVOR_SET", "fallback")
	if got != "found" {
		t.Errorf("envOr = %q, want %q", got, "found")
	}
}

func TestPhase2_EnvOr_Unset(t *testing.T) {
	os.Unsetenv("SAGE_TEST_ENVOR_UNSET")
	got := envOr("SAGE_TEST_ENVOR_UNSET", "fallback")
	if got != "fallback" {
		t.Errorf("envOr = %q, want %q", got, "fallback")
	}
}

func TestPhase2_EnvOr_EmptyString(t *testing.T) {
	// An empty env var should return the default because the function
	// checks for v != "".
	t.Setenv("SAGE_TEST_ENVOR_EMPTY", "")
	got := envOr("SAGE_TEST_ENVOR_EMPTY", "default")
	if got != "default" {
		t.Errorf("envOr with empty = %q, want %q", got, "default")
	}
}

// ---------------------------------------------------------------------------
// envInt (50% coverage)
// ---------------------------------------------------------------------------

func TestPhase2_EnvInt_Valid(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVINT_VALID", "42")
	got := envInt("SAGE_TEST_ENVINT_VALID")
	if got != 42 {
		t.Errorf("envInt = %d, want 42", got)
	}
}

func TestPhase2_EnvInt_Unset(t *testing.T) {
	os.Unsetenv("SAGE_TEST_ENVINT_UNSET")
	got := envInt("SAGE_TEST_ENVINT_UNSET")
	if got != 0 {
		t.Errorf("envInt unset = %d, want 0", got)
	}
}

func TestPhase2_EnvInt_Invalid(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVINT_BAD", "notanumber")
	got := envInt("SAGE_TEST_ENVINT_BAD")
	if got != 0 {
		t.Errorf("envInt invalid = %d, want 0", got)
	}
}

func TestPhase2_EnvInt_Negative(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVINT_NEG", "-5")
	got := envInt("SAGE_TEST_ENVINT_NEG")
	if got != -5 {
		t.Errorf("envInt negative = %d, want -5", got)
	}
}

func TestPhase2_EnvInt_EmptyString(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVINT_EMPTY", "")
	got := envInt("SAGE_TEST_ENVINT_EMPTY")
	if got != 0 {
		t.Errorf("envInt empty = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// envFloat (0% coverage)
// ---------------------------------------------------------------------------

func TestPhase2_EnvFloat_Valid(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVFLOAT_VALID", "3.14")
	got := envFloat("SAGE_TEST_ENVFLOAT_VALID")
	if got != 3.14 {
		t.Errorf("envFloat = %f, want 3.14", got)
	}
}

func TestPhase2_EnvFloat_Unset(t *testing.T) {
	os.Unsetenv("SAGE_TEST_ENVFLOAT_UNSET")
	got := envFloat("SAGE_TEST_ENVFLOAT_UNSET")
	if got != 0 {
		t.Errorf("envFloat unset = %f, want 0", got)
	}
}

func TestPhase2_EnvFloat_Invalid(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVFLOAT_BAD", "notafloat")
	got := envFloat("SAGE_TEST_ENVFLOAT_BAD")
	if got != 0 {
		t.Errorf("envFloat invalid = %f, want 0", got)
	}
}

func TestPhase2_EnvFloat_Integer(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVFLOAT_INT", "42")
	got := envFloat("SAGE_TEST_ENVFLOAT_INT")
	if got != 42.0 {
		t.Errorf("envFloat integer = %f, want 42.0", got)
	}
}

func TestPhase2_EnvFloat_Negative(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVFLOAT_NEG", "-2.5")
	got := envFloat("SAGE_TEST_ENVFLOAT_NEG")
	if got != -2.5 {
		t.Errorf("envFloat negative = %f, want -2.5", got)
	}
}

func TestPhase2_EnvFloat_EmptyString(t *testing.T) {
	t.Setenv("SAGE_TEST_ENVFLOAT_EMPTY", "")
	got := envFloat("SAGE_TEST_ENVFLOAT_EMPTY")
	if got != 0 {
		t.Errorf("envFloat empty = %f, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// HotReloadable (0% coverage)
// ---------------------------------------------------------------------------

func TestPhase2_HotReloadable_ReturnsNonEmpty(t *testing.T) {
	cfg := &Config{}
	fields := cfg.HotReloadable()
	if len(fields) == 0 {
		t.Fatal("HotReloadable() returned empty slice")
	}
	// Verify expected fields are present.
	expected := []string{
		"collector.interval_seconds",
		"analyzer.*",
		"safety.*",
		"trust.level",
		"llm.*",
		"retention.*",
	}
	for _, exp := range expected {
		found := false
		for _, f := range fields {
			if f == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("HotReloadable() missing %q", exp)
		}
	}
}

func TestPhase2_HotReloadable_DoesNotContainPostgres(t *testing.T) {
	cfg := &Config{}
	for _, f := range cfg.HotReloadable() {
		if f == "postgres.*" || f == "postgres.host" {
			t.Errorf("HotReloadable() should not contain postgres fields, found %q", f)
		}
	}
}

// ---------------------------------------------------------------------------
// IsStandalone (0% coverage)
// ---------------------------------------------------------------------------

func TestPhase2_IsStandalone_True(t *testing.T) {
	cfg := &Config{Mode: "standalone"}
	if !cfg.IsStandalone() {
		t.Error("IsStandalone() = false for standalone mode")
	}
}

func TestPhase2_IsStandalone_False(t *testing.T) {
	for _, mode := range []string{"extension", "fleet", ""} {
		cfg := &Config{Mode: mode}
		if cfg.IsStandalone() {
			t.Errorf("IsStandalone() = true for mode %q", mode)
		}
	}
}

// ---------------------------------------------------------------------------
// RateLimit (0% coverage)
// ---------------------------------------------------------------------------

func TestPhase2_RateLimit_Default(t *testing.T) {
	os.Unsetenv("SAGE_RATE_LIMIT")
	cfg := &Config{}
	got := cfg.RateLimit()
	if got != DefaultRateLimit {
		t.Errorf("RateLimit() = %d, want default %d", got, DefaultRateLimit)
	}
}

func TestPhase2_RateLimit_EnvOverride(t *testing.T) {
	t.Setenv("SAGE_RATE_LIMIT", "100")
	cfg := &Config{}
	got := cfg.RateLimit()
	if got != 100 {
		t.Errorf("RateLimit() = %d, want 100", got)
	}
}

func TestPhase2_RateLimit_InvalidEnv(t *testing.T) {
	t.Setenv("SAGE_RATE_LIMIT", "not-a-number")
	cfg := &Config{}
	got := cfg.RateLimit()
	if got != DefaultRateLimit {
		t.Errorf("RateLimit() with invalid env = %d, want default %d",
			got, DefaultRateLimit)
	}
}

// ---------------------------------------------------------------------------
// overlayEnv (59.2% coverage — need more env var branches)
// ---------------------------------------------------------------------------

func TestPhase2_OverlayEnv_PGPort(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PG_PORT", "5433")
	overlayEnv(cfg)
	if cfg.Postgres.Port != 5433 {
		t.Errorf("Postgres.Port = %d, want 5433", cfg.Postgres.Port)
	}
}

func TestPhase2_OverlayEnv_PGMaxConns(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PG_MAX_CONNS", "10")
	overlayEnv(cfg)
	if cfg.Postgres.MaxConnections != 10 {
		t.Errorf("MaxConnections = %d, want 10",
			cfg.Postgres.MaxConnections)
	}
}

func TestPhase2_OverlayEnv_PGUser(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PG_USER", "testuser")
	overlayEnv(cfg)
	if cfg.Postgres.User != "testuser" {
		t.Errorf("Postgres.User = %q, want %q",
			cfg.Postgres.User, "testuser")
	}
}

func TestPhase2_OverlayEnv_PGPassword(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PG_PASSWORD", "secret123")
	overlayEnv(cfg)
	if cfg.Postgres.Password != "secret123" {
		t.Errorf("Postgres.Password = %q, want %q",
			cfg.Postgres.Password, "secret123")
	}
}

func TestPhase2_OverlayEnv_PGDatabase(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PG_DATABASE", "testdb")
	overlayEnv(cfg)
	if cfg.Postgres.Database != "testdb" {
		t.Errorf("Postgres.Database = %q, want %q",
			cfg.Postgres.Database, "testdb")
	}
}

func TestPhase2_OverlayEnv_PGSSLMode(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PG_SSLMODE", "require")
	overlayEnv(cfg)
	if cfg.Postgres.SSLMode != "require" {
		t.Errorf("Postgres.SSLMode = %q, want %q",
			cfg.Postgres.SSLMode, "require")
	}
}

func TestPhase2_OverlayEnv_PrometheusPort(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PROMETHEUS_PORT", "9999")
	overlayEnv(cfg)
	if cfg.Prometheus.ListenAddr != "0.0.0.0:9999" {
		t.Errorf("ListenAddr = %q, want %q",
			cfg.Prometheus.ListenAddr, "0.0.0.0:9999")
	}
}

func TestPhase2_OverlayEnv_LLMFields(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_LLM_API_KEY", "sk-test")
	t.Setenv("SAGE_LLM_ENDPOINT", "https://api.example.com")
	t.Setenv("SAGE_LLM_MODEL", "gpt-4o")
	overlayEnv(cfg)
	if cfg.LLM.APIKey != "sk-test" {
		t.Errorf("LLM.APIKey = %q", cfg.LLM.APIKey)
	}
	if cfg.LLM.Endpoint != "https://api.example.com" {
		t.Errorf("LLM.Endpoint = %q", cfg.LLM.Endpoint)
	}
	if cfg.LLM.Model != "gpt-4o" {
		t.Errorf("LLM.Model = %q", cfg.LLM.Model)
	}
}

func TestPhase2_OverlayEnv_TrustLevel(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_TRUST_LEVEL", "advisory")
	overlayEnv(cfg)
	if cfg.Trust.Level != "advisory" {
		t.Errorf("Trust.Level = %q, want advisory", cfg.Trust.Level)
	}
}

func TestPhase2_OverlayEnv_MetaDB(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_META_DB", "postgres://meta@host/db")
	overlayEnv(cfg)
	if cfg.MetaDB != "postgres://meta@host/db" {
		t.Errorf("MetaDB = %q", cfg.MetaDB)
	}
}

func TestPhase2_OverlayEnv_EncryptionKey(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_ENCRYPTION_KEY", "enc-key-123")
	overlayEnv(cfg)
	if cfg.EncryptionKey != "enc-key-123" {
		t.Errorf("EncryptionKey = %q", cfg.EncryptionKey)
	}
}

func TestPhase2_OverlayEnv_OptimizerLLM(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_OPTIMIZER_LLM_API_KEY", "opt-key")
	t.Setenv("SAGE_OPTIMIZER_LLM_ENDPOINT", "https://opt.example.com")
	t.Setenv("SAGE_OPTIMIZER_LLM_MODEL", "o1-pro")
	overlayEnv(cfg)
	if cfg.LLM.OptimizerLLM.APIKey != "opt-key" {
		t.Errorf("OptimizerLLM.APIKey = %q", cfg.LLM.OptimizerLLM.APIKey)
	}
	if cfg.LLM.OptimizerLLM.Endpoint != "https://opt.example.com" {
		t.Errorf("OptimizerLLM.Endpoint = %q",
			cfg.LLM.OptimizerLLM.Endpoint)
	}
	if cfg.LLM.OptimizerLLM.Model != "o1-pro" {
		t.Errorf("OptimizerLLM.Model = %q",
			cfg.LLM.OptimizerLLM.Model)
	}
}

func TestPhase2_OverlayEnv_OAuth(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_OAUTH_CLIENT_ID", "client-123")
	t.Setenv("SAGE_OAUTH_CLIENT_SECRET", "secret-456")
	t.Setenv("SAGE_OAUTH_ISSUER_URL", "https://issuer.example.com")
	t.Setenv("SAGE_OAUTH_REDIRECT_URL", "https://redirect.example.com")
	t.Setenv("SAGE_OAUTH_PROVIDER", "google")
	overlayEnv(cfg)
	if cfg.OAuth.ClientID != "client-123" {
		t.Errorf("OAuth.ClientID = %q", cfg.OAuth.ClientID)
	}
	if cfg.OAuth.ClientSecret != "secret-456" {
		t.Errorf("OAuth.ClientSecret = %q", cfg.OAuth.ClientSecret)
	}
	if cfg.OAuth.IssuerURL != "https://issuer.example.com" {
		t.Errorf("OAuth.IssuerURL = %q", cfg.OAuth.IssuerURL)
	}
	if cfg.OAuth.RedirectURL != "https://redirect.example.com" {
		t.Errorf("OAuth.RedirectURL = %q", cfg.OAuth.RedirectURL)
	}
	if cfg.OAuth.Provider != "google" {
		t.Errorf("OAuth.Provider = %q", cfg.OAuth.Provider)
	}
}

func TestPhase2_OverlayEnv_Mode(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_MODE", "fleet")
	overlayEnv(cfg)
	if cfg.Mode != "fleet" {
		t.Errorf("Mode = %q, want fleet", cfg.Mode)
	}
}

func TestPhase2_OverlayEnv_DatabaseURL(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_DATABASE_URL", "postgres://u:p@h:5432/db")
	overlayEnv(cfg)
	if cfg.Postgres.DatabaseURL != "postgres://u:p@h:5432/db" {
		t.Errorf("DatabaseURL = %q", cfg.Postgres.DatabaseURL)
	}
}

func TestPhase2_OverlayEnv_PGHost(t *testing.T) {
	cfg := newDefaults()
	t.Setenv("SAGE_PG_HOST", "remote-host")
	overlayEnv(cfg)
	if cfg.Postgres.Host != "remote-host" {
		t.Errorf("Postgres.Host = %q, want remote-host",
			cfg.Postgres.Host)
	}
}

// ---------------------------------------------------------------------------
// applyHotReload (44.6% coverage — exercise more field branches)
// ---------------------------------------------------------------------------

func TestPhase2_ApplyHotReload_AnalyzerFields(t *testing.T) {
	target := newDefaults()
	target.Analyzer.IntervalSeconds = 600
	target.Analyzer.SlowQueryThresholdMs = 1000

	fresh := newDefaults()
	fresh.Analyzer.IntervalSeconds = 300
	fresh.Analyzer.SlowQueryThresholdMs = 500

	changed := applyHotReload(target, fresh)

	if target.Analyzer.IntervalSeconds != 300 {
		t.Errorf("Analyzer.IntervalSeconds = %d, want 300",
			target.Analyzer.IntervalSeconds)
	}
	if target.Analyzer.SlowQueryThresholdMs != 500 {
		t.Errorf("Analyzer.SlowQueryThresholdMs = %d, want 500",
			target.Analyzer.SlowQueryThresholdMs)
	}

	expectChanged := map[string]bool{
		"analyzer.interval_seconds":       true,
		"analyzer.slow_query_threshold_ms": true,
	}
	for _, c := range changed {
		delete(expectChanged, c)
	}
	for missing := range expectChanged {
		t.Errorf("missing from changed list: %s", missing)
	}
}

func TestPhase2_ApplyHotReload_SafetyCPU(t *testing.T) {
	target := newDefaults()
	target.Safety.CPUCeilingPct = 90

	fresh := newDefaults()
	fresh.Safety.CPUCeilingPct = 75

	changed := applyHotReload(target, fresh)

	if target.Safety.CPUCeilingPct != 75 {
		t.Errorf("Safety.CPUCeilingPct = %d, want 75",
			target.Safety.CPUCeilingPct)
	}
	found := false
	for _, c := range changed {
		if c == "safety.cpu_ceiling_pct" {
			found = true
		}
	}
	if !found {
		t.Error("safety.cpu_ceiling_pct not in changed list")
	}
}

func TestPhase2_ApplyHotReload_TrustMaintenanceWindow(t *testing.T) {
	target := newDefaults()
	target.Trust.MaintenanceWindow = ""

	fresh := newDefaults()
	fresh.Trust.MaintenanceWindow = "02:00-05:00"

	changed := applyHotReload(target, fresh)

	if target.Trust.MaintenanceWindow != "02:00-05:00" {
		t.Errorf("MaintenanceWindow = %q, want 02:00-05:00",
			target.Trust.MaintenanceWindow)
	}
	found := false
	for _, c := range changed {
		if c == "trust.maintenance_window" {
			found = true
		}
	}
	if !found {
		t.Error("trust.maintenance_window not in changed list")
	}
}

func TestPhase2_ApplyHotReload_TrustTier3Bools(t *testing.T) {
	target := newDefaults()
	target.Trust.Tier3Safe = true
	target.Trust.Tier3Moderate = false

	fresh := newDefaults()
	fresh.Trust.Tier3Safe = false
	fresh.Trust.Tier3Moderate = true

	applyHotReload(target, fresh)

	if target.Trust.Tier3Safe != false {
		t.Error("Tier3Safe should be false after reload")
	}
	if target.Trust.Tier3Moderate != true {
		t.Error("Tier3Moderate should be true after reload")
	}
}

func TestPhase2_ApplyHotReload_LLMFields(t *testing.T) {
	target := newDefaults()
	target.LLM.Enabled = false
	target.LLM.Endpoint = "https://old.example.com"
	target.LLM.Model = "old-model"

	fresh := newDefaults()
	fresh.LLM.Enabled = true
	fresh.LLM.Endpoint = "https://new.example.com"
	fresh.LLM.Model = "new-model"

	changed := applyHotReload(target, fresh)

	if !target.LLM.Enabled {
		t.Error("LLM.Enabled should be true")
	}
	if target.LLM.Endpoint != "https://new.example.com" {
		t.Errorf("LLM.Endpoint = %q", target.LLM.Endpoint)
	}
	if target.LLM.Model != "new-model" {
		t.Errorf("LLM.Model = %q", target.LLM.Model)
	}

	expectChanged := map[string]bool{
		"llm.enabled":  true,
		"llm.endpoint": true,
		"llm.model":    true,
	}
	for _, c := range changed {
		delete(expectChanged, c)
	}
	for missing := range expectChanged {
		t.Errorf("missing from changed: %s", missing)
	}
}

func TestPhase2_ApplyHotReload_RetentionSnapshots(t *testing.T) {
	target := newDefaults()
	target.Retention.SnapshotsDays = 90

	fresh := newDefaults()
	fresh.Retention.SnapshotsDays = 30

	changed := applyHotReload(target, fresh)

	if target.Retention.SnapshotsDays != 30 {
		t.Errorf("SnapshotsDays = %d, want 30",
			target.Retention.SnapshotsDays)
	}
	found := false
	for _, c := range changed {
		if c == "retention.snapshots_days" {
			found = true
		}
	}
	if !found {
		t.Error("retention.snapshots_days not in changed list")
	}
}

func TestPhase2_ApplyHotReload_AlertingFields(t *testing.T) {
	target := newDefaults()
	target.Alerting.CooldownMinutes = 15
	target.Alerting.QuietHoursStart = ""
	target.Alerting.QuietHoursEnd = ""
	target.Alerting.CheckIntervalSeconds = 60
	target.Alerting.Enabled = false

	fresh := newDefaults()
	fresh.Alerting.CooldownMinutes = 30
	fresh.Alerting.QuietHoursStart = "22:00"
	fresh.Alerting.QuietHoursEnd = "06:00"
	fresh.Alerting.CheckIntervalSeconds = 120
	fresh.Alerting.Enabled = true
	fresh.Alerting.Routes = []AlertRoute{{Severity: "critical", Channels: []string{"slack"}}}
	fresh.Alerting.Webhooks = []WebhookConfig{{Name: "test", URL: "https://hook.example.com"}}

	changed := applyHotReload(target, fresh)

	if target.Alerting.CooldownMinutes != 30 {
		t.Errorf("CooldownMinutes = %d, want 30",
			target.Alerting.CooldownMinutes)
	}
	if target.Alerting.QuietHoursStart != "22:00" {
		t.Errorf("QuietHoursStart = %q", target.Alerting.QuietHoursStart)
	}
	if target.Alerting.QuietHoursEnd != "06:00" {
		t.Errorf("QuietHoursEnd = %q", target.Alerting.QuietHoursEnd)
	}
	if target.Alerting.CheckIntervalSeconds != 120 {
		t.Errorf("CheckIntervalSeconds = %d",
			target.Alerting.CheckIntervalSeconds)
	}
	if !target.Alerting.Enabled {
		t.Error("Alerting.Enabled should be true")
	}
	if len(target.Alerting.Routes) != 1 {
		t.Errorf("Routes len = %d, want 1", len(target.Alerting.Routes))
	}
	if len(target.Alerting.Webhooks) != 1 {
		t.Errorf("Webhooks len = %d, want 1",
			len(target.Alerting.Webhooks))
	}

	expectChanged := map[string]bool{
		"alerting.cooldown_minutes":       true,
		"alerting.quiet_hours_start":      true,
		"alerting.quiet_hours_end":        true,
		"alerting.check_interval_seconds": true,
		"alerting.enabled":                true,
	}
	for _, c := range changed {
		delete(expectChanged, c)
	}
	for missing := range expectChanged {
		t.Errorf("missing from changed: %s", missing)
	}
}

func TestPhase2_ApplyHotReload_TunerFields(t *testing.T) {
	target := newDefaults()
	target.Tuner.Enabled = true
	target.Tuner.WorkMemMaxMB = 512
	target.Tuner.PlanTimeRatio = 3.0
	target.Tuner.NestedLoopRowThreshold = 10000
	target.Tuner.ParallelMinTableRows = 1000000
	target.Tuner.MinQueryCalls = 100
	target.Tuner.VerifyAfterApply = true

	fresh := newDefaults()
	fresh.Tuner.Enabled = false
	fresh.Tuner.WorkMemMaxMB = 256
	fresh.Tuner.PlanTimeRatio = 5.0
	fresh.Tuner.NestedLoopRowThreshold = 5000
	fresh.Tuner.ParallelMinTableRows = 500000
	fresh.Tuner.MinQueryCalls = 50
	fresh.Tuner.VerifyAfterApply = false

	changed := applyHotReload(target, fresh)

	if target.Tuner.Enabled != false {
		t.Error("Tuner.Enabled should be false")
	}
	if target.Tuner.WorkMemMaxMB != 256 {
		t.Errorf("WorkMemMaxMB = %d", target.Tuner.WorkMemMaxMB)
	}
	if target.Tuner.PlanTimeRatio != 5.0 {
		t.Errorf("PlanTimeRatio = %f", target.Tuner.PlanTimeRatio)
	}
	if target.Tuner.NestedLoopRowThreshold != 5000 {
		t.Errorf("NestedLoopRowThreshold = %d",
			target.Tuner.NestedLoopRowThreshold)
	}
	if target.Tuner.ParallelMinTableRows != 500000 {
		t.Errorf("ParallelMinTableRows = %d",
			target.Tuner.ParallelMinTableRows)
	}
	if target.Tuner.MinQueryCalls != 50 {
		t.Errorf("MinQueryCalls = %d", target.Tuner.MinQueryCalls)
	}
	if target.Tuner.VerifyAfterApply != false {
		t.Error("VerifyAfterApply should be false")
	}

	expectChanged := map[string]bool{
		"tuner.enabled":                   true,
		"tuner.work_mem_max_mb":           true,
		"tuner.plan_time_ratio":           true,
		"tuner.nested_loop_row_threshold": true,
		"tuner.parallel_min_table_rows":   true,
		"tuner.min_query_calls":           true,
		"tuner.verify_after_apply":        true,
	}
	for _, c := range changed {
		delete(expectChanged, c)
	}
	for missing := range expectChanged {
		t.Errorf("missing from changed: %s", missing)
	}
}

func TestPhase2_ApplyHotReload_AutoExplainFields(t *testing.T) {
	target := newDefaults()
	target.AutoExplain.LogMinDurationMs = 1000
	target.AutoExplain.MaxPlansPerCycle = 100

	fresh := newDefaults()
	fresh.AutoExplain.LogMinDurationMs = 500
	fresh.AutoExplain.MaxPlansPerCycle = 50

	changed := applyHotReload(target, fresh)

	if target.AutoExplain.LogMinDurationMs != 500 {
		t.Errorf("LogMinDurationMs = %d", target.AutoExplain.LogMinDurationMs)
	}
	if target.AutoExplain.MaxPlansPerCycle != 50 {
		t.Errorf("MaxPlansPerCycle = %d",
			target.AutoExplain.MaxPlansPerCycle)
	}

	expectChanged := map[string]bool{
		"auto_explain.log_min_duration_ms": true,
		"auto_explain.max_plans_per_cycle": true,
	}
	for _, c := range changed {
		delete(expectChanged, c)
	}
	for missing := range expectChanged {
		t.Errorf("missing from changed: %s", missing)
	}
}

func TestPhase2_ApplyHotReload_BatchSize(t *testing.T) {
	target := newDefaults()
	target.Collector.BatchSize = 1000

	fresh := newDefaults()
	fresh.Collector.BatchSize = 2000

	changed := applyHotReload(target, fresh)

	if target.Collector.BatchSize != 2000 {
		t.Errorf("BatchSize = %d, want 2000", target.Collector.BatchSize)
	}
	found := false
	for _, c := range changed {
		if c == "collector.batch_size" {
			found = true
		}
	}
	if !found {
		t.Error("collector.batch_size not in changed list")
	}
}

func TestPhase2_ApplyHotReload_NoChanges(t *testing.T) {
	target := newDefaults()
	fresh := newDefaults()

	changed := applyHotReload(target, fresh)
	if len(changed) != 0 {
		t.Errorf("expected no changes, got %v", changed)
	}
}

// ---------------------------------------------------------------------------
// Load YAML with env var expansion
// ---------------------------------------------------------------------------

func TestPhase2_LoadYAML_EnvExpansion(t *testing.T) {
	// Clear the live-env override so the YAML-expanded value survives.
	// Load() reads SAGE_LLM_API_KEY at config.go:917 and clobbers whatever
	// came from the YAML, so this test must isolate from the ambient env.
	t.Setenv("SAGE_LLM_API_KEY", "")
	t.Setenv("SAGE_TEST_LLM_KEY", "test-api-key")
	tmp := t.TempDir()
	yamlContent := `mode: extension
llm:
  api_key: "${SAGE_TEST_LLM_KEY}"
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load([]string{"--config=" + cfgPath})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.LLM.APIKey != "test-api-key" {
		t.Errorf("LLM.APIKey = %q, want %q",
			cfg.LLM.APIKey, "test-api-key")
	}
}

func TestPhase2_LoadYAML_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	_, err := Load([]string{"--config=/nonexistent/path/config.yaml"})
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}

// ---------------------------------------------------------------------------
// Validation edge cases
// ---------------------------------------------------------------------------

func TestPhase2_Validate_NegativeSlowQueryThreshold(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `analyzer:
  slow_query_threshold_ms: -1
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	os.WriteFile(cfgPath, []byte(yamlContent), 0644)

	_, err := Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for negative slow_query_threshold_ms")
	}
}

func TestPhase2_Validate_CPUCeilingOver100(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `safety:
  cpu_ceiling_pct: 101
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	os.WriteFile(cfgPath, []byte(yamlContent), 0644)

	_, err := Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for cpu_ceiling_pct > 100")
	}
}

func TestPhase2_Validate_ZeroBatchSize(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `collector:
  batch_size: 0
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	os.WriteFile(cfgPath, []byte(yamlContent), 0644)

	_, err := Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for zero batch_size")
	}
}

func TestPhase2_Validate_ZeroQueryTimeout(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `safety:
  query_timeout_ms: 0
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	os.WriteFile(cfgPath, []byte(yamlContent), 0644)

	_, err := Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for zero query_timeout_ms")
	}
}

func TestPhase2_Validate_ZeroAnalyzerInterval(t *testing.T) {
	tmp := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `analyzer:
  interval_seconds: 0
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	os.WriteFile(cfgPath, []byte(yamlContent), 0644)

	_, err := Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for zero analyzer interval")
	}
}
