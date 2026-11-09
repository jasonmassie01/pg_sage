package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// Default paths of the generated tables, relative to sidecar/.
const (
	defaultEnvDocs     = "../docs/configuration.md"
	defaultContribDocs = "../CONTRIBUTING.md"
)

// envTable is one generated environment-variable table and the flag that
// names its file.
type envTable struct {
	flag, defaultPath, what string
	render                  func() string
	section, replace        func(string) (string, error)
}

var (
	runtimeEnvTable = envTable{flag: "-env-docs", defaultPath: defaultEnvDocs,
		what: "runtime", render: config.EnvVarsMarkdown,
		section: config.EnvDocsSection, replace: config.ReplaceEnvDocsSection}
	contributorEnvTable = envTable{flag: "-contrib-docs", defaultPath: defaultContribDocs,
		what: "contributor", render: config.ContributorEnvVarsMarkdown,
		section: config.ContributorEnvDocsSection,
		replace: config.ReplaceContributorEnvDocsSection}
)

func (tb envTable) regenerate() string {
	return "regenerate with `go run ./cmd/gen_config_meta -env-only " + tb.flag + " " +
		tb.defaultPath + "`"
}

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

// write regenerates the table's section of path in place, keeping the
// file's line endings.
func (tb envTable) write(path string) error {
	doc, crlf, err := readEnvDoc(path)
	if err != nil {
		return err
	}
	updated, err := tb.replace(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if crlf {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write env docs %s: %w", path, err)
	}
	rows := strings.Count(tb.render(), "\n") - 2
	fmt.Fprintf(os.Stderr, "gen_config_meta: wrote %d %s environment variables to %s\n",
		rows, tb.what, path)
	return nil
}

// check fails when the table's section of path differs from the registry;
// it writes nothing.
func (tb envTable) check(path string) error {
	doc, _, err := readEnvDoc(path)
	if err != nil {
		return err
	}
	section, err := tb.section(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if section != tb.render() {
		return fmt.Errorf("%s: the %s environment-variable table has drifted from "+
			"internal/config/env_registry.go; %s", path, tb.what, tb.regenerate())
	}
	fmt.Fprintf(os.Stderr, "gen_config_meta: %s %s env table matches the registry\n",
		path, tb.what)
	return nil
}

// writeEnvDocs regenerates each table whose path is set.
func writeEnvDocs(envDocs, contribDocs string) error {
	if envDocs != "" {
		if err := runtimeEnvTable.write(envDocs); err != nil {
			return err
		}
	}
	if contribDocs != "" {
		return contributorEnvTable.write(contribDocs)
	}
	return nil
}

// checkEnvDocs checks each table whose path is set; with neither set it
// checks both default files.
func checkEnvDocs(envDocs, contribDocs string) error {
	if envDocs == "" && contribDocs == "" {
		envDocs, contribDocs = defaultEnvDocs, defaultContribDocs
	}
	if envDocs != "" {
		if err := runtimeEnvTable.check(envDocs); err != nil {
			return err
		}
	}
	if contribDocs != "" {
		return contributorEnvTable.check(contribDocs)
	}
	return nil
}
