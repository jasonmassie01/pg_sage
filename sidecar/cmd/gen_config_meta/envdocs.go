package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// defaultEnvDocs is the configuration guide, relative to sidecar/.
const defaultEnvDocs = "../docs/configuration.md"

const envDocsRegenerate = "regenerate with `go run ./cmd/gen_config_meta -env-only " +
	"-env-docs " + defaultEnvDocs + "`"

// readEnvDoc returns path's content with LF line endings, and whether the
// file used CRLF.
func readEnvDoc(path string) (string, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read env docs %s: %w", path, err)
	}
	text := string(body)
	crlf := strings.Contains(text, "\r\n")
	return strings.ReplaceAll(text, "\r\n", "\n"), crlf, nil
}

// writeEnvDocs regenerates the env-vars section of path in place, keeping
// the file's line endings.
func writeEnvDocs(path string) error {
	doc, crlf, err := readEnvDoc(path)
	if err != nil {
		return err
	}
	updated, err := config.ReplaceEnvDocsSection(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if crlf {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write env docs %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "gen_config_meta: wrote %d environment variables to %s\n",
		len(config.EnvVars()), path)
	return nil
}

// checkEnvDocs fails when the env-vars section of path (default
// defaultEnvDocs) differs from the registry; it writes nothing.
func checkEnvDocs(path string) error {
	if path == "" {
		path = defaultEnvDocs
	}
	doc, _, err := readEnvDoc(path)
	if err != nil {
		return err
	}
	section, err := config.EnvDocsSection(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if section != config.EnvVarsMarkdown() {
		return fmt.Errorf("%s: the environment-variable table has drifted from "+
			"internal/config/env_registry.go; %s", path, envDocsRegenerate)
	}
	fmt.Fprintf(os.Stderr, "gen_config_meta: %s env table matches the registry\n", path)
	return nil
}
