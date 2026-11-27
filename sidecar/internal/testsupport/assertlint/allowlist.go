package assertlint

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ParseAllowlist reads allowlist lines of the form "<rule> <path> <func>".
// Blank lines and lines starting with # are ignored.
func ParseAllowlist(r io.Reader) ([]string, error) {
	var keys []string
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("allowlist line %d: want \"<rule> <path> <func>\", got %q",
				n, line)
		}
		switch Rule(fields[0]) {
		case RuleNoAssert:
		case RuleGetenv:
			return nil, fmt.Errorf("allowlist line %d: a getenv finding cannot be allowlisted; "+
				"read the DSN with testdb.SkipUnlessLive(t): %s", n, line)
		default:
			return nil, fmt.Errorf("allowlist line %d: unknown rule %q (want %s)",
				n, fields[0], RuleNoAssert)
		}
		key := strings.Join(fields, " ")
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("allowlist line %d: duplicate of line %d: %s", n, prev, key)
		}
		seen[key] = n
		keys = append(keys, key)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read allowlist: %w", err)
	}
	return keys, nil
}

// Compare splits findings into those the allowlist does not name, and
// returns the allowlist entries no finding matches. A stale entry means
// the test now asserts, moved or was deleted: the list may only shrink.
func Compare(findings []Finding, allow []string) (unlisted []Finding, stale []string) {
	listed := map[string]bool{}
	for _, k := range allow {
		listed[k] = true
	}
	matched := map[string]bool{}
	for _, f := range findings {
		if listed[f.Key()] {
			matched[f.Key()] = true
			continue
		}
		unlisted = append(unlisted, f)
	}
	for _, k := range allow {
		if !matched[k] {
			stale = append(stale, k)
		}
	}
	return unlisted, stale
}
