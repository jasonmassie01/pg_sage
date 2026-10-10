package main

import (
	"os"

	"github.com/pg-sage/sidecar/internal/config"
)

// secretGetenv resolves names through config.LookupSecretEnv, so NAME_FILE
// works for them (GR-11), and returns a getenv that serves the resolved
// values and falls back to os.Getenv for every other name.
func secretGetenv(names ...string) (func(string) string, error) {
	resolved := make(map[string]string, len(names))
	for _, name := range names {
		value, err := config.LookupSecretEnv(name)
		if err != nil {
			return nil, err
		}
		resolved[name] = value
	}
	return func(name string) string {
		if value, ok := resolved[name]; ok {
			return value
		}
		return os.Getenv(name)
	}, nil
}
