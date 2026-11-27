package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
)

// The environment overlay applies SAGE_* variables over the YAML values
// (config-08). A variable is applied when it is set and non-empty, so an
// explicit 0 or false is a value; one that is set but cannot be parsed is
// an error that names it, and Load refuses to start rather than run on the
// file's value. Range checks stay with validate.

// lookupEnv reads key and parses it. ok is false when key is unset or
// empty; err names key and its raw value when parsing fails.
func lookupEnv[T any](key string, parse func(string) (T, error)) (T, bool, error) {
	var zero T
	raw := os.Getenv(key)
	if raw == "" {
		return zero, false, nil
	}
	value, err := parse(raw)
	if err != nil {
		return zero, false, fmt.Errorf("%s=%q: %w", key, raw, err)
	}
	return value, true, nil
}

func parseEnvInt(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("not an integer")
	}
	return n, nil
}

func parseEnvFloat(raw string) (float64, error) {
	f, err := strconv.ParseFloat(raw, 64)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("not a finite number")
	}
	if err != nil {
		return 0, errors.New("not a number")
	}
	return f, nil
}

func parseEnvBool(raw string) (bool, error) {
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("not a boolean (use true or false)")
	}
	return b, nil
}

func parseEnvPort(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("not a port (an integer from 1 to 65535)")
	}
	return n, nil
}

func parseEnvListenAddr(raw string) (string, error) {
	if err := checkListenAddr(raw, 1); err != nil {
		return "", errors.New("not a host:port address with a port from 1 to 65535")
	}
	return raw, nil
}

// envOverlay collects the parse errors of one overlay pass.
type envOverlay struct{ errs []string }

// overlayVar sets *dst from key when it is set and parses.
func overlayVar[T any](o *envOverlay, key string, parse func(string) (T, error), dst *T) {
	value, ok, err := lookupEnv(key, parse)
	if err != nil {
		o.errs = append(o.errs, err.Error())
		return
	}
	if ok {
		*dst = value
	}
}

// setStrings sets each target from its variable when that is non-empty.
func (o *envOverlay) setStrings(targets []envString) {
	for _, target := range targets {
		if v := os.Getenv(target.key); v != "" {
			*target.dst = v
		}
	}
}

// check only validates key: it is read where it is used, not stored.
func (o *envOverlay) check(key string, parse func(string) (int, error)) {
	var discard int
	overlayVar(o, key, parse, &discard)
}

func (o *envOverlay) err() error {
	if len(o.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid environment: %s (fix or unset them)",
		strings.Join(o.errs, "; "))
}

type envString struct {
	key string
	dst *string
}

func overlayEnv(cfg *Config) error {
	o := &envOverlay{}
	o.setStrings([]envString{
		{"SAGE_MODE", &cfg.Mode},
		{"SAGE_DATABASE_URL", &cfg.Postgres.DatabaseURL},
		{"SAGE_PG_HOST", &cfg.Postgres.Host},
	})
	overlayVar(o, "SAGE_PG_PORT", parseEnvInt, &cfg.Postgres.Port)
	o.setStrings([]envString{
		{"SAGE_PG_USER", &cfg.Postgres.User},
		{"SAGE_PG_PASSWORD", &cfg.Postgres.Password},
		{"SAGE_PG_DATABASE", &cfg.Postgres.Database},
		{"SAGE_PG_SSLMODE", &cfg.Postgres.SSLMode},
	})
	overlayVar(o, "SAGE_PG_MAX_CONNS", parseEnvInt, &cfg.Postgres.MaxConnections)
	overlayPrometheusPort(o, cfg)
	o.setStrings(coreSecretAndLLMEnv(cfg))
	overlayAgentNativeEnv(o, cfg)
	// Read by RateLimit when the API starts; checked here so junk stops Load.
	o.check("SAGE_RATE_LIMIT", parseEnvInt)
	return o.err()
}

func coreSecretAndLLMEnv(cfg *Config) []envString {
	return []envString{
		{"SAGE_LLM_API_KEY", &cfg.LLM.APIKey},
		{"SAGE_LLM_AUTH", &cfg.LLM.Auth},
		{"SAGE_LLM_ENDPOINT", &cfg.LLM.Endpoint},
		{"SAGE_LLM_MODEL", &cfg.LLM.Model},
		{"SAGE_TRUST_LEVEL", &cfg.Trust.Level},
		{"SAGE_META_DB", &cfg.MetaDB},
		{"SAGE_ENCRYPTION_KEY", &cfg.EncryptionKey},
		{"SAGE_ENCRYPTION_KEY_PREVIOUS", &cfg.EncryptionKeyPrevious},
		{"SAGE_DATA_DIR", &cfg.DataDir},
		{"SAGE_BREAK_GLASS_PASSWORD_HASH", &cfg.OAuth.BreakGlass.PasswordHash},
		{"SAGE_OPTIMIZER_LLM_API_KEY", &cfg.LLM.OptimizerLLM.APIKey},
		{"SAGE_OPTIMIZER_LLM_ENDPOINT", &cfg.LLM.OptimizerLLM.Endpoint},
		{"SAGE_OPTIMIZER_LLM_MODEL", &cfg.LLM.OptimizerLLM.Model},
		{"SAGE_REVIEW_LLM_API_KEY", &cfg.LLM.ReviewLLM.APIKey},
		{"SAGE_REVIEW_LLM_ENDPOINT", &cfg.LLM.ReviewLLM.Endpoint},
		{"SAGE_REVIEW_LLM_MODEL", &cfg.LLM.ReviewLLM.Model},
		{"SAGE_OAUTH_CLIENT_ID", &cfg.OAuth.ClientID},
		{"SAGE_OAUTH_CLIENT_SECRET", &cfg.OAuth.ClientSecret},
		{"SAGE_OAUTH_ISSUER_URL", &cfg.OAuth.IssuerURL},
		{"SAGE_OAUTH_REDIRECT_URL", &cfg.OAuth.RedirectURL},
		{"SAGE_OAUTH_PROVIDER", &cfg.OAuth.Provider},
	}
}

// overlayPrometheusPort replaces only the port of the metrics address. The
// host stays what the YAML (or the loopback default) chose: a port variable
// never widens the bind to every interface. SAGE_PROMETHEUS_ADDR, the whole
// address, wins when both are set (observability-08).
func overlayPrometheusPort(o *envOverlay, cfg *Config) {
	port, ok, err := lookupEnv("SAGE_PROMETHEUS_PORT", parseEnvPort)
	if err != nil {
		o.errs = append(o.errs, err.Error())
	}
	addr, addrOK, addrErr := lookupEnv("SAGE_PROMETHEUS_ADDR", parseEnvListenAddr)
	if addrErr != nil {
		o.errs = append(o.errs, addrErr.Error())
	}
	if addrOK {
		cfg.Prometheus.ListenAddr = addr
		return
	}
	if !ok {
		return
	}
	host, _, err := net.SplitHostPort(cfg.Prometheus.ListenAddr)
	if err != nil {
		host, _, _ = net.SplitHostPort(DefaultPrometheusListenAddr)
	}
	cfg.Prometheus.ListenAddr = net.JoinHostPort(host, strconv.Itoa(port))
}

func overlayAgentNativeEnv(o *envOverlay, cfg *Config) {
	o.setStrings([]envString{{"SAGE_POLICY_PROFILE", &cfg.Policy.Profile}})
	overlayVar(o, "SAGE_VALUE_TOIL_MODEL_VERSION", parseEnvInt, &cfg.Value.ToilModelVersion)
	overlayVar(o, "SAGE_VERIFY_WINDOW_MINUTES", parseEnvInt, &cfg.Verify.WindowMinutes)
	overlayVar(o, "SAGE_VERIFY_WINDOW_MAX_MINUTES", parseEnvInt, &cfg.Verify.WindowMaxMinutes)
	overlayVar(o, "SAGE_VERIFY_MIN_GAIN_PCT", parseEnvFloat, &cfg.Verify.MinGainPct)
	overlayVar(o, "SAGE_VERIFY_REGRESS_PCT", parseEnvFloat, &cfg.Verify.RegressPct)
	overlayVar(o, "SAGE_VERIFY_WRITE_IMPACT_PCT", parseEnvFloat, &cfg.Verify.WriteImpactPct)
	overlayVar(o, "SAGE_VERIFY_MIN_SAMPLES", parseEnvInt, &cfg.Verify.MinSamples)
	o.setStrings([]envString{
		{"SAGE_CLONE_PROVIDER", &cfg.Clone.Provider},
		{"SAGE_CLONE_DLE_ENDPOINT", &cfg.Clone.DLEEndpoint},
		{"SAGE_CLONE_DLE_TOKEN", &cfg.Clone.DLEToken},
		{"SAGE_CLONE_NEON_API_KEY", &cfg.Clone.Neon.APIKey},
	})
	overlayVar(o, "SAGE_CLONE_MAX_AGE_MINUTES", parseEnvInt, &cfg.Clone.MaxCloneAgeMinutes)
	overlayVar(o, "SAGE_CUSTODIAN_FREEZE_RED_BUFFER_PCT", parseEnvFloat,
		&cfg.Custodian.Freeze.RedBufferPct)
	overlayVar(o, "SAGE_CUSTODIAN_WAL_ABANDON_AFTER_MINUTES", parseEnvInt,
		&cfg.Custodian.WAL.AbandonAfterMinutes)
	overlayVar(o, "SAGE_CUSTODIAN_WAL_DISK_PCT_CEILING", parseEnvFloat,
		&cfg.Custodian.WAL.RetainedWALDiskPctCeiling)
	overlayVar(o, "SAGE_MCP_ENABLED", parseEnvBool, &cfg.MCP.Enabled)
	o.setStrings([]envString{{"SAGE_MCP_TRANSPORT", &cfg.MCP.Transport}})
	overlayAzureEnv(cfg)
	overlayCloudTelemetryEnv(o, cfg)
	overlayAPIOriginsEnv(cfg)
}

// RateLimit returns the configured rate limit: SAGE_RATE_LIMIT when it is
// a positive integer (Load already refused one that does not parse),
// otherwise the default.
func (c *Config) RateLimit() int {
	if v, ok, err := lookupEnv("SAGE_RATE_LIMIT", parseEnvInt); err == nil && ok && v > 0 {
		return v
	}
	return DefaultRateLimit
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
