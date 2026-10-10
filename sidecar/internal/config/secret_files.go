package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
)

// GR-11: pg_sage reads each of its own secrets either from NAME or from the
// file named by NAME_FILE, which any secrets manager can fill (Vault Agent,
// CSI drivers, External Secrets, Docker secrets). No vendor SDKs.
//
// Rules:
//   - NAME and NAME_FILE both non-empty is an error: there is no silent winner.
//   - The file's content is the value, minus one trailing newline ("\n" or
//     "\r\n"); an empty value is an error.
//   - A file readable or writable by everyone draws a warning (not on Windows,
//     whose permission bits say nothing about ACLs).
//   - ${NAME} references in the YAML honour NAME_FILE the same way.
//   - CLI flags still win over both.

// maxSecretFileBytes bounds a secret file; anything larger is the wrong file.
const maxSecretFileBytes = 64 << 10

// secretFileSuffix names the companion variable that holds a file path.
const secretFileSuffix = "_FILE"

// secretEnvTargets maps each secret variable Load applies to its field.
var secretEnvTargets = []struct {
	name  string
	field func(*Config) *string
}{
	{"SAGE_DATABASE_URL", func(c *Config) *string { return &c.Postgres.DatabaseURL }},
	{"SAGE_PG_PASSWORD", func(c *Config) *string { return &c.Postgres.Password }},
	{"SAGE_META_DB", func(c *Config) *string { return &c.MetaDB }},
	{"SAGE_ENCRYPTION_KEY", func(c *Config) *string { return &c.EncryptionKey }},
	{"SAGE_LLM_API_KEY", func(c *Config) *string { return &c.LLM.APIKey }},
	{"SAGE_OPTIMIZER_LLM_API_KEY", func(c *Config) *string { return &c.LLM.OptimizerLLM.APIKey }},
	{"SAGE_OAUTH_CLIENT_SECRET", func(c *Config) *string { return &c.OAuth.ClientSecret }},
	{"SAGE_CLONE_DLE_TOKEN", func(c *Config) *string { return &c.Clone.DLEToken }},
	{"SAGE_API_KEY", func(c *Config) *string { return &c.APIKey }},
}

// runtimeSecretEnvNames are secret variables read outside Load, through
// LookupSecretEnv, by the process that needs them.
var runtimeSecretEnvNames = []string{
	"SAGE_SUPABASE_OBSERVABILITY_TOKEN",
	"SAGE_VECTORLAB_DATABASE_URL",
	"SAGE_HISTORY_MONITORED_DSN",
}

// SecretEnvNames lists every environment variable through which pg_sage
// reads one of its own secrets; each also accepts NAME_FILE.
func SecretEnvNames() []string {
	names := make([]string, 0, len(secretEnvTargets)+len(runtimeSecretEnvNames))
	for _, target := range secretEnvTargets {
		names = append(names, target.name)
	}
	return append(names, runtimeSecretEnvNames...)
}

// LookupSecretEnv returns the value of name, read from the file named by
// name_FILE when that is set instead. It is empty when neither is set.
func LookupSecretEnv(name string) (string, error) {
	return lookupSecretEnvWith(name, os.Stat, runtime.GOOS)
}

func lookupSecretEnvWith(
	name string, stat func(string) (fs.FileInfo, error), goos string,
) (string, error) {
	fileVar := name + secretFileSuffix
	value, path := os.Getenv(name), os.Getenv(fileVar)
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("%s and %s are both set; set only one of them",
			name, fileVar)
	}
	if info, err := stat(path); err == nil && secretFileTooOpen(info, goos) {
		_, _ = fmt.Fprintf(configWarningOutput, "WARNING: %s names %q, which every "+
			"user on this host can read or write; chmod 600 (or 400) it\n", fileVar, path)
	}
	return readSecretFile(fileVar, path)
}

// readSecretFile reads one secret file, trimming one trailing newline. The
// errors name the variable and path, never the content.
func readSecretFile(fileVar, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", fileVar, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s: reading %q: %w", fileVar, path, err)
	}
	if len(data) > maxSecretFileBytes {
		return "", fmt.Errorf("%s: %q is larger than %d bytes; is it the right file?",
			fileVar, path, maxSecretFileBytes)
	}
	value := string(data)
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	if value == "" {
		return "", fmt.Errorf("%s: %q is empty", fileVar, path)
	}
	return value, nil
}

// secretFileTooOpen reports a file other users can read or write. Group
// access is allowed: Kubernetes secret volumes are group-readable by fsGroup.
func secretFileTooOpen(info fs.FileInfo, goos string) bool {
	if goos == "windows" {
		return false
	}
	return info.Mode().Perm()&0o006 != 0
}

// applySecretFiles sets each secret field whose value comes from a NAME_FILE
// variable. Plain NAME values were already applied by overlayEnv.
func applySecretFiles(cfg *Config) error {
	var errs []error
	for _, target := range secretEnvTargets {
		if os.Getenv(target.name+secretFileSuffix) == "" {
			continue
		}
		value, err := LookupSecretEnv(target.name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		*target.field(cfg) = value
	}
	for _, name := range runtimeSecretEnvNames {
		// Fail at startup, not when the feature first reads it.
		if _, err := LookupSecretEnv(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// bracedEnvValue resolves a ${NAME} reference in the YAML: NAME, or the file
// named by NAME_FILE. checkBracedSecretFiles already reported any error and
// permission warning, so this lookup is quiet and an error expands to "".
func bracedEnvValue(name string) string {
	quiet := func(string) (fs.FileInfo, error) { return nil, fs.ErrInvalid }
	value, err := lookupSecretEnvWith(name, quiet, runtime.GOOS)
	if err != nil {
		return ""
	}
	return value
}

// checkBracedSecretFiles validates every NAME_FILE behind a ${NAME} reference
// in raw before expansion, so a conflict or unreadable file fails the load.
func checkBracedSecretFiles(raw string) error {
	var errs []error
	seen := map[string]bool{}
	for _, name := range bracedEnvNames(raw) {
		if seen[name] || os.Getenv(name+secretFileSuffix) == "" {
			continue
		}
		seen[name] = true
		if _, err := LookupSecretEnv(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
