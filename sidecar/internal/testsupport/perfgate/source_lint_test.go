package perfgate

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// volatileCutoff matches a time-window predicate bounded by the volatile
// clock_timestamp(): "col > clock_timestamp() - make_interval(...)".
// PostgreSQL cannot use a volatile expression as an index bound, so such a
// predicate reads the whole history table however small the window is.
// now() is stable within a statement and serves the same purpose.
var volatileCutoff = regexp.MustCompile(`(?i)[<>]=?\s*(pg_catalog\.)?clock_timestamp\(\)` +
	`\s*-\s*(pg_catalog\.)?make_interval`)

// TestNoVolatileTimeWindowPredicates is a permanent guard found by the
// performance gate (runway samples, query store, action log and Sage SRE
// investigation windows all scanned sequentially).
func TestNoVolatileTimeWindowPredicates(t *testing.T) {
	var offenders []string
	for _, root := range []string{"../..", "../../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, loc := range volatileCutoff.FindAllIndex(src, -1) {
				line := 1 + strings.Count(string(src[:loc[0]]), "\n")
				offenders = append(offenders, filepath.ToSlash(path)+":"+strconv.Itoa(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("time-window predicates bounded by clock_timestamp() (use now()):\n%s",
			strings.Join(offenders, "\n"))
	}
}

func TestVolatileCutoffPattern(t *testing.T) {
	hit := []string{
		"WHERE sampled_at < clock_timestamp() - make_interval(secs => $1)",
		"AND q.captured_at >= pg_catalog.clock_timestamp()\n  - pg_catalog.make_interval(secs => $2)",
	}
	miss := []string{
		"WHERE sampled_at < now() - make_interval(secs => $1)",
		"EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - l.executed_at)",
		"AND lease_until > clock_timestamp()",
		"DEFAULT clock_timestamp()",
	}
	for _, s := range hit {
		if !volatileCutoff.MatchString(s) {
			t.Errorf("not flagged: %q", s)
		}
	}
	for _, s := range miss {
		if volatileCutoff.MatchString(s) {
			t.Errorf("flagged: %q", s)
		}
	}
}
