package docscontract

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mkdocsConfig is the part of mkdocs.yml these checks read.
type mkdocsConfig struct {
	DocsDir    string                       `yaml:"docs_dir"`
	Nav        []any                        `yaml:"nav"`
	NotInNav   string                       `yaml:"not_in_nav"`
	Validation map[string]map[string]string `yaml:"validation"`
}

// internalNotePattern names files that are working notes, not user docs:
// agent session logs and handoffs, release plans, superpowers plans and
// the reverse-engineered spec.
var internalNotePattern = regexp.MustCompile(
	`codex-|handoff|plan_v|(^|/)superpowers/|(^|/)reverse_spec/`)

// repoRoot is the repository root, three levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// parseMkdocs decodes mkdocs.yml and rejects a config without a nav, so a
// YAML refactor that loses the nav cannot pass the checks trivially.
func parseMkdocs(data []byte) (mkdocsConfig, error) {
	var cfg mkdocsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse mkdocs.yml: %w", err)
	}
	if len(cfg.Nav) == 0 {
		return cfg, errors.New("mkdocs.yml has no nav entries")
	}
	if cfg.DocsDir == "" {
		cfg.DocsDir = "docs"
	}
	return cfg, nil
}

// loadMkdocs reads and parses the repository's mkdocs.yml.
func loadMkdocs(t *testing.T) (mkdocsConfig, string) {
	t.Helper()
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "mkdocs.yml"))
	if err != nil {
		t.Fatalf("read mkdocs.yml: %v", err)
	}
	cfg, err := parseMkdocs(data)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, filepath.Join(root, cfg.DocsDir)
}

// navFiles flattens the nav into the page paths it names. External links
// are skipped; any other shape is an error naming the entry.
func navFiles(nav []any) ([]string, error) {
	var out []string
	for _, entry := range nav {
		files, err := navEntryFiles(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}

func navEntryFiles(entry any) ([]string, error) {
	switch v := entry.(type) {
	case string:
		if strings.Contains(v, "://") {
			return nil, nil
		}
		return []string{v}, nil
	case []any:
		return navFiles(v)
	case map[string]any:
		var out []string
		for _, child := range v {
			files, err := navEntryFiles(child)
			if err != nil {
				return nil, err
			}
			out = append(out, files...)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected nav entry %#v (%T)", entry, entry)
}

// docsPages lists every Markdown file under dir as a slash path relative
// to dir, sorted.
func docsPages(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".md" {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}

// missingNavFiles returns the nav entries with no file under dir.
func missingNavFiles(dir string, entries []string) []string {
	var missing []string
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(e)))
		if err != nil || info.IsDir() {
			missing = append(missing, e)
		}
	}
	return missing
}

// gitignoreMatch reports whether rel matches one of the newline-separated
// patterns, the way MkDocs reads not_in_nav: blank lines and # comments
// are skipped, a pattern containing a slash is anchored at the docs root,
// and one without a slash matches a file name at any depth.
func gitignoreMatch(patterns, rel string) bool {
	for _, line := range strings.Split(patterns, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(rel, strings.TrimPrefix(p, "/")) {
				return true
			}
			continue
		}
		if !strings.Contains(p, "/") {
			if ok, _ := path.Match(p, path.Base(rel)); ok {
				return true
			}
			continue
		}
		if ok, _ := path.Match(strings.TrimPrefix(p, "/"), rel); ok {
			return true
		}
	}
	return false
}

// isInternalNote reports whether a docs-relative path is a working note.
func isInternalNote(rel string) bool {
	return internalNotePattern.MatchString(rel)
}
