package config

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Owner-check bounds (market-11).
const (
	MinCheckIntervalMinutes     = 5
	DefaultCheckIntervalMinutes = 60
	MaxCheckRows                = 100
	MaxCheckTimeoutMs           = 5000
	DefaultCheckTimeoutMs       = 1000
)

// CustomCheck is one owner-defined read-only check: a single SELECT whose
// rows each become a custom:<id> finding keyed by object_column.
type CustomCheck struct {
	ID              string   `yaml:"id" doc:"Check id: lower case letters, digits and _; the finding category is custom:<id>."`
	Title           string   `yaml:"title" doc:"Finding title; the row's object is appended."`
	Severity        string   `yaml:"severity" doc:"info, warning or critical."`
	SQL             string   `yaml:"sql" doc:"One plain SELECT without parameters. It runs in a READ ONLY transaction."`
	Databases       []string `yaml:"databases" doc:"Database name globs the check runs on. Default: every database."`
	IntervalMinutes int      `yaml:"interval_minutes" doc:"Minutes between runs, at least 5. Default: 60."`
	MaxRows         int      `yaml:"max_rows" doc:"Most rows read, 1-100; more rows mark the run truncated. Default: 100."`
	TimeoutMs       int      `yaml:"timeout_ms" doc:"statement_timeout of the check, 1-5000 ms. Default: 1000."`
	ObjectColumn    string   `yaml:"object_column" doc:"Result column naming the finding's object."`
	Message         string   `yaml:"message" doc:"Recommendation text shown on each finding."`
}

// PreparedCheck is a check whose SQL the parser accepted: Canonical is what
// runs, and Functions are the called names the first run resolves in
// pg_proc (functions and x.name attribute calls), lower case.
type PreparedCheck struct {
	CustomCheck
	Canonical   string
	Fingerprint string
	Functions   []string
}

// Category is the finding category of the check.
func (c CustomCheck) Category() string { return "custom:" + c.ID }

// AppliesTo reports whether the check runs on database (glob match; a
// pattern that does not parse never matches, and validation refuses it).
func (c CustomCheck) AppliesTo(database string) bool {
	for _, pattern := range c.Databases {
		if ok, err := path.Match(pattern, database); err == nil && ok {
			return true
		}
	}
	return false
}

func (c CustomCheck) withDefaults() CustomCheck {
	if c.IntervalMinutes == 0 {
		c.IntervalMinutes = DefaultCheckIntervalMinutes
	}
	if c.MaxRows == 0 {
		c.MaxRows = MaxCheckRows
	}
	if c.TimeoutMs == 0 {
		c.TimeoutMs = DefaultCheckTimeoutMs
	}
	if len(c.Databases) == 0 {
		c.Databases = []string{"*"}
	}
	return c
}

var checkIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

var checkSeverities = map[string]bool{"info": true, "warning": true, "critical": true}

// validateCheckShape checks everything but the SQL text's meaning.
func validateCheckShape(field string, c CustomCheck) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{field}, args...)...)
	}
	switch {
	case !checkIDPattern.MatchString(c.ID):
		return bad("id %q must be lower case letters, digits and _ (at most 63)", c.ID)
	case strings.TrimSpace(c.Title) == "":
		return bad("title is empty")
	case !checkSeverities[c.Severity]:
		return bad("severity %q must be info, warning or critical", c.Severity)
	case strings.TrimSpace(c.SQL) == "":
		return bad("sql is empty")
	case strings.TrimSpace(c.ObjectColumn) == "":
		return bad("object_column is empty")
	case c.IntervalMinutes < MinCheckIntervalMinutes:
		return bad("interval_minutes must be at least %d, got %d",
			MinCheckIntervalMinutes, c.IntervalMinutes)
	case c.MaxRows < 1 || c.MaxRows > MaxCheckRows:
		return bad("max_rows must be 1-%d, got %d", MaxCheckRows, c.MaxRows)
	case c.TimeoutMs < 1 || c.TimeoutMs > MaxCheckTimeoutMs:
		return bad("timeout_ms must be 1-%d, got %d", MaxCheckTimeoutMs, c.TimeoutMs)
	}
	for _, pattern := range c.Databases {
		if _, err := path.Match(pattern, ""); err != nil {
			return bad("databases glob %q does not parse: %v", pattern, err)
		}
	}
	return nil
}

// deniedFunctionPrefixes are refused whatever their volatility: they sleep,
// reach other servers, or read server files and large objects.
var deniedFunctionPrefixes = []string{"pg_sleep", "dblink", "lo_", "pg_read_", "pg_ls_"}

// DeniedCheckFunction reports whether a called function is never allowed
// in an owner check.
func DeniedCheckFunction(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range deniedFunctionPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// PrepareCheck validates one check and parses its SQL: exactly one plain
// SELECT (no write, FOR UPDATE, INTO or second statement), no parameter
// and no denied function. A build without the parser refuses every check
// (sqlast.ErrUnavailable): it never runs SQL it did not parse.
func PrepareCheck(c CustomCheck) (PreparedCheck, error) {
	c = c.withDefaults()
	field := fmt.Sprintf("check %q", c.ID)
	if err := validateCheckShape(field, c); err != nil {
		return PreparedCheck{}, err
	}
	read, err := sqlast.InspectBrokeredRead(c.SQL)
	if errors.Is(err, sqlast.ErrUnavailable) {
		return PreparedCheck{}, fmt.Errorf("%s: owner checks need the SQL parser, "+
			"which this build lacks (use a release build): %w", field, err)
	}
	if err != nil {
		return PreparedCheck{}, fmt.Errorf("%s: sql refused: %w", field, err)
	}
	if read.Params != 0 {
		return PreparedCheck{}, fmt.Errorf("%s: sql refused: parameters ($%d) "+
			"are not allowed", field, read.Params)
	}
	p := PreparedCheck{CustomCheck: c, Canonical: read.Canonical,
		Fingerprint: read.Fingerprint}
	for _, list := range [][]sqlast.QualifiedName{read.Query.Functions,
		read.Query.AttributeCalls} {
		for _, fn := range list {
			if DeniedCheckFunction(fn.Name) {
				return PreparedCheck{}, fmt.Errorf("%s: sql refused: function %s "+
					"is not allowed in a check", field, fn)
			}
			p.Functions = append(p.Functions, strings.ToLower(fn.Name))
		}
	}
	return p, nil
}

// PrepareChecks prepares every check, refusing a duplicate id. Shape
// errors are reported before the SQL is parsed.
func PrepareChecks(checks []CustomCheck) ([]PreparedCheck, error) {
	seen := map[string]bool{}
	for i, c := range checks {
		if seen[c.ID] {
			return nil, fmt.Errorf("checks[%d]: duplicate id %q", i, c.ID)
		}
		seen[c.ID] = true
		field := fmt.Sprintf("checks[%d]", i)
		if err := validateCheckShape(field, c.withDefaults()); err != nil {
			return nil, err
		}
	}
	out := make([]PreparedCheck, 0, len(checks))
	for i, c := range checks {
		p, err := PrepareCheck(c)
		if err != nil {
			return nil, fmt.Errorf("checks[%d]: %w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}
