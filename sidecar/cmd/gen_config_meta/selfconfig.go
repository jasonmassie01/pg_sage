package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// keyClassFor is the self-configuration class of a metadata key; an
// element field of a list (alerting.routes[].x) takes its list's class.
func keyClassFor(key string) string {
	if class, ok := config.KeyClassOf(key); ok {
		return string(class)
	}
	if i := strings.Index(key, "[]"); i > 0 {
		if class, ok := config.KeyClassOf(key[:i]); ok {
			return string(class)
		}
	}
	return ""
}

func writeDerivedReference(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDir(path), err)
	}
	if err := os.WriteFile(path, []byte(selfconfig.Markdown()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "gen_config_meta: wrote derived settings to %s\n", path)
	return nil
}
