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

// TestExpandBracedEnv_PreservesBareDollar is the H5 regression: a literal
// '$' in a secret written directly into YAML must survive expansion.
// os.ExpandEnv treated p@ss$word as p@ss + $word (unset → ""), silently
// corrupting the credential. expandBracedEnv expands only ${NAME}.
func TestExpandBracedEnv_PreservesBareDollar(t *testing.T) {
	t.Setenv("SAGE_TEST_PW_VAR", "fromenv")
	os.Unsetenv("SAGE_TEST_DEFINITELY_UNSET_999")
	cases := []struct{ name, in, want string }{
		{"braced set expands", "pw: ${SAGE_TEST_PW_VAR}", "pw: fromenv"},
		{"bare dollar in password kept", "pw: p@ss$word", "pw: p@ss$word"},
		{"bare dollar before word kept", "k: s3cr$tValue", "k: s3cr$tValue"},
		{"trailing bare dollar kept", "k: secret$", "k: secret$"},
		{"unset braced -> empty", "pw: ${SAGE_TEST_DEFINITELY_UNSET_999}", "pw: "},
		{"mixed braced and bare", "a: ${SAGE_TEST_PW_VAR} b: c$d", "a: fromenv b: c$d"},
	}
	for _, c := range cases {
		if got := expandBracedEnv(c.in); got != c.want {
			t.Errorf("%s: expandBracedEnv(%q) = %q, want %q",
				c.name, c.in, got, c.want)
		}
	}
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

func TestWave2_RateLimit_NonPositiveEnvUsesDefault(t *testing.T) {
	for _, value := range []string{"0", "-1", "-60"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SAGE_RATE_LIMIT", value)
			if got := (&Config{}).RateLimit(); got != DefaultRateLimit {
				t.Fatalf("RateLimit() with %q = %d, want default %d",
					value, got, DefaultRateLimit)
			}
		})
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
