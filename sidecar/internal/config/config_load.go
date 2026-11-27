package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// braceEnvRe matches ${NAME} references only (NAME starts with a letter
// or underscore). Bare $VAR is deliberately NOT matched.
var braceEnvRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandBracedEnv expands only ${NAME} references (set → value, unset →
// empty string, matching the documented "empty when unset" behavior). A
// NAME_FILE variable supplies the value from a file (GR-11).
// Unlike os.ExpandEnv it leaves bare '$' untouched, so a literal '$' in
// a password or API key written directly into YAML is no longer
// silently truncated (H5). Used by both initial load and hot-reload.
func expandBracedEnv(raw string) string {
	return braceEnvRe.ReplaceAllStringFunc(raw, func(match string) string {
		name := braceEnvRe.FindStringSubmatch(match)[1]
		return bracedEnvValue(name)
	})
}

// Load reads config from YAML file, then overlays env vars, then CLI flags.
// Precedence: CLI > env > YAML > defaults. Persisted UI overrides replay on
// top of the result, except that trust ceilings take the most restrictive
// of an explicit file value and the override (config-03).
func Load(args []string) (*Config, error) {
	cfg := newDefaults()
	// Mode starts unset so inferMode can tell an explicit choice (YAML,
	// SAGE_MODE, --mode) from the built-in default.
	cfg.Mode = ""

	flags, err := parseFlags(args)
	if err != nil {
		return nil, err
	}
	if err := readConfigFile(cfg, *flags.configPath); err != nil {
		return nil, err
	}
	if err := applyEnvOverlay(cfg); err != nil {
		return nil, err
	}
	flags.apply(cfg)
	applyLegacyEnv(cfg)
	if err := finalizeMode(cfg); err != nil {
		return nil, err
	}
	if err := cfg.validateFileConfig(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	cfg.Safety.warnInheritedHostCPUCeiling()

	return cfg, nil
}

// loadFlags holds the command-line flags Load reads, parsed early so the
// config path and mode are known before the file is read.
type loadFlags struct {
	fs            *flag.FlagSet
	configPath    *string
	mode          *string
	pgHost        *string
	pgPort        *int
	pgUser        *string
	pgPassword    *string
	pgDatabase    *string
	pgSSLMode     *string
	pgURL         *string
	promAddr      *string
	metaDB        *string
	encryptionKey *string
}

// parseFlags parses the command line into loadFlags.
func parseFlags(args []string) (*loadFlags, error) {
	fs := flag.NewFlagSet("pg_sage_sidecar", flag.ContinueOnError)
	f := &loadFlags{
		fs:            fs,
		configPath:    fs.String("config", "", "Path to config.yaml"),
		mode:          fs.String("mode", "", "Operating mode: standalone, fleet, or meta"),
		pgHost:        fs.String("pg-host", "", "PostgreSQL host"),
		pgPort:        fs.Int("pg-port", 0, "PostgreSQL port"),
		pgUser:        fs.String("pg-user", "", "PostgreSQL user"),
		pgPassword:    fs.String("pg-password", "", "PostgreSQL password"),
		pgDatabase:    fs.String("pg-database", "", "PostgreSQL database"),
		pgSSLMode:     fs.String("pg-sslmode", "", "PostgreSQL sslmode"),
		pgURL:         fs.String("pg-url", "", pgURLUsage),
		promAddr:      fs.String("prom-addr", "", "Prometheus listen address"),
		metaDB:        fs.String("meta-db", "", "Metadata database connection string"),
		encryptionKey: fs.String("encryption-key", "", "Passphrase for credential encryption"),
	}
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("parse flags: %w", err)
	}
	return f, nil
}

const pgURLUsage = "PostgreSQL connection URL (overrides individual pg-* flags)"

// readConfigFile loads the YAML file named by --config, SAGE_CONFIG_PATH or
// a config.yaml in the working directory, if any.
func readConfigFile(cfg *Config, configPath string) error {
	yamlPath := configPath
	if yamlPath == "" {
		yamlPath = envOr("SAGE_CONFIG_PATH", "")
	}
	if yamlPath == "" {
		// Auto-detect config.yaml in CWD.
		if _, err := os.Stat("config.yaml"); err == nil {
			yamlPath = "config.yaml"
		}
	}
	if yamlPath == "" {
		return nil
	}
	if err := loadYAML(yamlPath, cfg); err != nil {
		return fmt.Errorf("loading config %s: %w", yamlPath, err)
	}
	cfg.ConfigPath = yamlPath
	return nil
}

// applyEnvOverlay overlays environment variables, then NAME_FILE secrets.
func applyEnvOverlay(cfg *Config) error {
	if err := overlayEnv(cfg); err != nil {
		return err
	}
	if err := applySecretFiles(cfg); err != nil {
		return err
	}
	markEnvPaths(cfg)
	return nil
}

// apply overlays the CLI flags (highest precedence).
func (f *loadFlags) apply(cfg *Config) {
	setString(&cfg.Mode, *f.mode)
	setString(&cfg.Postgres.Host, *f.pgHost)
	if *f.pgPort != 0 {
		cfg.Postgres.Port = *f.pgPort
	}
	setString(&cfg.Postgres.User, *f.pgUser)
	setString(&cfg.Postgres.Password, *f.pgPassword)
	setString(&cfg.Postgres.Database, *f.pgDatabase)
	setString(&cfg.Postgres.SSLMode, *f.pgSSLMode)
	setString(&cfg.Postgres.DatabaseURL, *f.pgURL)
	setString(&cfg.Prometheus.ListenAddr, *f.promAddr)
	setString(&cfg.MetaDB, *f.metaDB)
	setString(&cfg.EncryptionKey, *f.encryptionKey)
	markFlagPaths(cfg, f.fs)
}

// setString overwrites dst with a non-empty flag value.
func setString(dst *string, value string) {
	if value != "" {
		*dst = value
	}
}

// applyLegacyEnv applies the legacy environment variables.
func applyLegacyEnv(cfg *Config) {
	if v := os.Getenv("SAGE_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	cfg.TLSCert = os.Getenv("SAGE_TLS_CERT")
	cfg.TLSKey = os.Getenv("SAGE_TLS_KEY")
}

// finalizeMode infers an unset mode, normalizes fleet/standalone config
// and rejects an invalid mode, before the validators run.
func finalizeMode(cfg *Config) error {
	if cfg.Mode == "" {
		cfg.Mode = inferMode(cfg)
	}
	cfg.normalize()
	return validateMode(cfg)
}

func loadYAML(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Expand ${ENV_VAR} references with validation. Only the braced
	// form is expanded; a bare '$' (e.g. in a password) is left intact.
	raw := string(data)
	if err := checkBracedSecretFiles(raw); err != nil {
		return err
	}
	expanded := expandBracedEnv(raw)

	// Warn about env vars that expanded to empty strings. This catches the
	// common case where ${SAGE_LLM_API_KEY} is in the YAML but the env var
	// is not set, leaving an empty value that silently breaks the feature.
	for _, warning := range unexpandedEnvWarnings(path, raw) {
		_, _ = fmt.Fprintln(configWarningOutput, warning)
	}
	if err := requireOneDocument(expanded); err != nil {
		return err
	}
	if err := rejectRetiredTopLevelConfig(expanded); err != nil {
		return err
	}
	expanded, err = dropRetired(expanded)
	if err != nil {
		return err
	}

	candidate := Clone(cfg)
	decoder := yaml.NewDecoder(strings.NewReader(expanded))
	decoder.KnownFields(true)
	// An empty file is a file with no settings: the defaults stand.
	if err := decoder.Decode(candidate); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := migrateLegacyIndexOptimizer(expanded, candidate); err != nil {
		return err
	}
	if candidate.explicitPaths, err = yamlPresencePaths(expanded); err != nil {
		return err
	}
	*cfg = *candidate
	return nil
}

func rejectRetiredTopLevelConfig(raw string) error {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return err
	}
	if len(document.Content) == 0 ||
		len(document.Content[0].Content) == 0 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "notifications":
			return fmt.Errorf(
				"configuration key %q is retired; use %q instead",
				"notifications", "alerting",
			)
		}
	}
	return nil
}

// configWarningOutput receives configuration warnings (stderr in
// production; tests capture it).
var configWarningOutput io.Writer = os.Stderr

// unexpandedEnvWarnings lists ${VAR} references in YAML scalar values whose
// variable is unset. Comments are not configuration and are ignored.
func unexpandedEnvWarnings(path, raw string) []string {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return nil // the strict decoder reports the syntax error
	}
	var warnings []string
	seen := map[string]bool{}
	walkScalars(&document, func(value string) {
		for _, name := range bracedEnvNames(value) {
			if seen[name] || os.Getenv(name) != "" ||
				os.Getenv(name+secretFileSuffix) != "" {
				continue
			}
			seen[name] = true
			warnings = append(warnings, fmt.Sprintf(
				"WARNING: config %q references ${%s} but it is not set "+
					"in the environment", path, name))
		}
	})
	return warnings
}

func walkScalars(node *yaml.Node, visit func(string)) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		visit(node.Value)
	}
	for _, child := range node.Content {
		walkScalars(child, visit)
	}
}

func bracedEnvNames(value string) []string {
	var names []string
	for _, match := range braceEnvRe.FindAllStringSubmatch(value, -1) {
		names = append(names, match[1])
	}
	return names
}

// inferMode picks the mode when none was configured. A meta database means
// a meta-db fleet (never standalone: standalone+meta registers a phantom
// instance); anything else monitors one database standalone (G10-B01).
func inferMode(cfg *Config) string {
	if cfg.MetaDB != "" {
		return ModeMeta
	}
	return DefaultMode
}
