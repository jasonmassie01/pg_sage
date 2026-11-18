package cases

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// Migration file names PR mode can follow: Flyway's V<n>__name.sql, and
// <n>_name.sql (or .up.sql/.down.sql pairs) where n is a sequence or a
// 14-digit UTC timestamp. Anything else is refused, never guessed.
var (
	flywayName   = regexp.MustCompile(`^V([0-9]+)__.+\.sql$`)
	numberedName = regexp.MustCompile(`^([0-9]+)_.+?(\.up\.sql|\.down\.sql|\.sql)$`)
	slugBreak    = regexp.MustCompile(`[^a-z0-9]+`)
)

const (
	timestampLayout = "20060102150405"
	maxSlug         = 48
)

// migrationNaming is the pattern the existing files follow.
type migrationNaming struct {
	kind   string // flyway, sequence or timestamp
	newest string // the newest file's number, as written
	upDown bool
}

func detectNaming(files []string) (migrationNaming, error) {
	var n migrationNaming
	var newest uint64
	for _, f := range files {
		kind, num, upDown := classifyMigration(f)
		if kind == "" {
			continue
		}
		if n.kind != "" && n.kind != kind {
			return migrationNaming{}, fmt.Errorf("%w: the files mix %s and %s names; "+
				"no single naming pattern to follow", errNoPR, n.kind, kind)
		}
		n.kind, n.upDown = kind, n.upDown || upDown
		v, err := strconv.ParseUint(num, 10, 64)
		if err == nil && (n.newest == "" || v > newest) {
			newest, n.newest = v, num
		}
	}
	if n.kind == "" || n.newest == "" {
		return migrationNaming{}, fmt.Errorf("%w: no migration naming pattern found "+
			"(V<n>__name.sql or <n>_name.sql)", errNoPR)
	}
	return n, nil
}

func classifyMigration(name string) (kind, num string, upDown bool) {
	if m := flywayName.FindStringSubmatch(name); m != nil {
		return "flyway", m[1], false
	}
	m := numberedName.FindStringSubmatch(name)
	if m == nil {
		return "", "", false
	}
	upDown = m[2] != ".sql"
	if len(m[1]) == len(timestampLayout) {
		return "timestamp", m[1], upDown
	}
	return "sequence", m[1], upDown
}

// nextMigrationNames names the new migration after the newest existing
// one: the up file, then the down file when the files come in pairs.
func nextMigrationNames(files []string, title string, now time.Time) ([]string, error) {
	n, err := detectNaming(files)
	if err != nil {
		return nil, err
	}
	slug := migrationSlug(title)
	var prefix string
	switch n.kind {
	case "flyway":
		v, _ := strconv.ParseUint(n.newest, 10, 64)
		return []string{fmt.Sprintf("V%d__%s.sql", v+1, slug)}, nil
	case "timestamp":
		ts := now.UTC()
		if last, err := time.Parse(timestampLayout, n.newest); err == nil && !ts.After(last) {
			ts = last.Add(time.Second)
		}
		prefix = ts.Format(timestampLayout)
	default:
		v, _ := strconv.ParseUint(n.newest, 10, 64)
		prefix = fmt.Sprintf("%0*d", len(n.newest), v+1)
	}
	if n.upDown {
		return []string{prefix + "_" + slug + ".up.sql", prefix + "_" + slug + ".down.sql"}, nil
	}
	return []string{prefix + "_" + slug + ".sql"}, nil
}

// migrationSlug is "pg_sage_" plus the title in lower-case words.
func migrationSlug(title string) string {
	s := strings.Trim(slugBreak.ReplaceAllString(strings.ToLower(title), "_"), "_")
	if len(s) > maxSlug {
		s = strings.TrimRight(s[:maxSlug], "_")
	}
	if s == "" {
		s = "change"
	}
	return "pg_sage_" + s
}

// migrationFiles writes up under dir as names[0] and, for a pair, the
// rollback (or a forward-fix note) as names[1].
func migrationFiles(dir string, names []string, up, down string) []codehost.FileChange {
	files := []codehost.FileChange{{Path: dir + "/" + names[0],
		Content: strings.TrimSpace(up) + "\n"}}
	if len(names) > 1 {
		body := strings.TrimSpace(down)
		if body == "" {
			body = "-- pg_sage: no rollback for this migration; it is a forward fix only."
		}
		files = append(files, codehost.FileChange{Path: dir + "/" + names[1],
			Content: body + "\n"})
	}
	return files
}

// replaceLiteral replaces the one verbatim occurrence of original, which
// must span line, with rewrite. A rewrite that adds a quote, backslash or
// newline the original lacks could break out of the literal: refused.
func replaceLiteral(content string, line int, original, rewrite string) (string, error) {
	switch {
	case strings.TrimSpace(rewrite) == "":
		return "", fmt.Errorf("%w: the rewrite is empty", errNoPR)
	case line < 1:
		return "", fmt.Errorf("%w: line %d is not a line of the file", errNoPR, line)
	case rewrite == original:
		return "", fmt.Errorf("%w: the rewrite is the same as the statement", errNoPR)
	}
	switch n := strings.Count(content, original); n {
	case 0:
		return "", fmt.Errorf("%w: the statement is not found verbatim in the file", errNoPR)
	case 1:
	default:
		return "", fmt.Errorf("%w: the statement appears %d times in the file", errNoPR, n)
	}
	at := strings.Index(content, original)
	start := 1 + strings.Count(content[:at], "\n")
	if end := start + strings.Count(original, "\n"); line < start || line > end {
		return "", fmt.Errorf("%w: the statement is at line %d, not at line %d", errNoPR,
			start, line)
	}
	if err := literalSafe(original, rewrite); err != nil {
		return "", err
	}
	return content[:at] + rewrite + content[at+len(original):], nil
}

func literalSafe(original, rewrite string) error {
	for _, q := range []string{`"`, "'", "`"} {
		if strings.Contains(rewrite, q) && !strings.Contains(original, q) {
			return fmt.Errorf("%w: the rewrite adds a %s quote the literal may not hold",
				errNoPR, q)
		}
	}
	if strings.Contains(rewrite, `\`) && !strings.Contains(original, `\`) {
		return fmt.Errorf("%w: the rewrite adds a backslash", errNoPR)
	}
	if strings.Contains(rewrite, "\n") && !strings.Contains(original, "\n") {
		return fmt.Errorf("%w: the rewrite adds a newline", errNoPR)
	}
	return nil
}
