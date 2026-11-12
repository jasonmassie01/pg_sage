package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// BenchResultsPathFromFile reads sre.autonomy.bench_results_path from a
// config file without loading the rest of it (benches-09: "pg_sage bench
// install" copies a verified report there and needs no database
// settings). ${VAR} references are expanded as config.Load expands them;
// a file without the key reads "".
func BenchResultsPathFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		SRE struct {
			Autonomy struct {
				BenchResultsPath string `yaml:"bench_results_path"`
			} `yaml:"autonomy"`
		} `yaml:"sre"`
	}
	if err := yaml.Unmarshal([]byte(expandBracedEnv(string(data))), &doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	return strings.TrimSpace(doc.SRE.Autonomy.BenchResultsPath), nil
}
