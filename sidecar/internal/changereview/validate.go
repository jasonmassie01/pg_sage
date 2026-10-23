package changereview

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/config"
)

const (
	maxPRURLBytes = 2048
	maxRepoRunes  = 300
	// clockSkew tolerates a CI clock slightly ahead of the database's.
	clockSkew = 5 * time.Minute
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// checkCaller refuses an empty, reserved or overlong actor and an unknown
// source.
func checkCaller(c Caller) error {
	if err := checkActor(c.Actor); err != nil {
		return err
	}
	switch c.Source {
	case SourceMCP, SourceAPI, SourceCLI:
		return nil
	}
	return invalid("source %q is not mcp, api or cli", c.Source)
}

func checkActor(actor string) error {
	a := strings.TrimSpace(actor)
	if a == "" || a == "*" || utf8.RuneCountInString(actor) > MaxActorRunes {
		return invalid("actor %q is empty, reserved or longer than %d characters", actor,
			MaxActorRunes)
	}
	return nil
}

// validateRequest checks r for database and returns it normalized: the
// database filled in, the tool lower-cased, the PR title trimmed and cut.
func validateRequest(cfg config.ChangeReviewConfig, database string, c Caller,
	r Request) (Request, error) {
	if err := checkCaller(c); err != nil {
		return r, err
	}
	if r.Database != "" && r.Database != database {
		return r, invalid("this service reviews database %q, not %q", database, r.Database)
	}
	r.Database = database
	if err := checkContent(cfg, r); err != nil {
		return r, err
	}
	tool, ok := LookupMigrationTool(r.MigrationTool)
	if !ok {
		return r, invalid("migration_tool %q is not one of %s", r.MigrationTool,
			strings.Join(MigrationToolNames()[1:], ", "))
	}
	r.MigrationTool = tool.Name
	if r.WaitSeconds < 0 || r.WaitSeconds > MaxWaitSeconds {
		return r, invalid("wait_seconds must be 0-%d, got %d", MaxWaitSeconds, r.WaitSeconds)
	}
	pr, err := checkPR(r.PR)
	r.PR = pr
	return r, err
}

// checkContent bounds the SQL and the diff and refuses text that is not
// valid UTF-8 or carries NUL bytes.
func checkContent(cfg config.ChangeReviewConfig, r Request) error {
	if len(r.SQLFiles) > MaxSQLFiles {
		return invalid("at most %d sql_files, got %d", MaxSQLFiles, len(r.SQLFiles))
	}
	total, empty := len(r.SQL), strings.TrimSpace(r.SQL) == ""
	if err := checkText("sql", r.SQL); err != nil {
		return err
	}
	for i, f := range r.SQLFiles {
		if f.Path == "" || len(f.Path) > MaxPathBytes || strings.IndexFunc(f.Path,
			unicode.IsControl) >= 0 || !utf8.ValidString(f.Path) {
			return invalid("sql_files[%d].path must be 1-%d bytes without control "+
				"characters", i, MaxPathBytes)
		}
		if err := checkText("sql_files["+f.Path+"]", f.Content); err != nil {
			return err
		}
		total += len(f.Content)
		empty = empty && strings.TrimSpace(f.Content) == ""
	}
	if total > cfg.MaxSQLBytes {
		return invalid("the SQL is %d bytes, over change_review.max_sql_bytes (%d)", total,
			cfg.MaxSQLBytes)
	}
	if len(r.Diff) > cfg.MaxDiffBytes {
		return invalid("the diff is %d bytes, over change_review.max_diff_bytes (%d)",
			len(r.Diff), cfg.MaxDiffBytes)
	}
	if empty && strings.TrimSpace(r.Diff) == "" {
		return invalid("a review needs sql or a diff")
	}
	return checkText("diff", r.Diff)
}

func checkText(what, s string) error {
	if !utf8.ValidString(s) {
		return invalid("%s is not valid UTF-8", what)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return invalid("%s contains a NUL byte", what)
	}
	return nil
}

// checkPR validates the PR metadata; every field is optional.
func checkPR(p PRInfo) (PRInfo, error) {
	if p.URL != "" {
		if err := checkPRURL(p.URL); err != nil {
			return p, err
		}
	}
	switch {
	case p.Number < 0:
		return p, invalid("pr.number must be positive, got %d", p.Number)
	case p.HeadSHA != "" && !shaPattern.MatchString(p.HeadSHA):
		return p, invalid("pr.head_sha must be 7-64 lower-case hex characters")
	case p.BaseSHA != "" && !shaPattern.MatchString(p.BaseSHA):
		return p, invalid("pr.base_sha must be 7-64 lower-case hex characters")
	case utf8.RuneCountInString(p.Repo) > maxRepoRunes || checkText("pr.repo", p.Repo) != nil:
		return p, invalid("pr.repo must be valid text of at most %d characters", maxRepoRunes)
	case p.OpenedAt != nil && p.OpenedAt.After(time.Now().Add(clockSkew)):
		return p, invalid("pr.opened_at %s is in the future", p.OpenedAt.Format(time.RFC3339))
	}
	if err := checkText("pr.title", p.Title); err != nil {
		return p, err
	}
	p.Title = clipRunes(strings.TrimSpace(p.Title), MaxPRTitleRunes)
	return p, nil
}

// checkPRURL accepts an https URL with a host and no credentials, without
// whitespace or control characters (agenttools' pr_url rules).
func checkPRURL(raw string) error {
	if len(raw) > maxPRURLBytes || strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return invalid("pr.url must be at most %d bytes without whitespace", maxPRURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalid("pr.url must be an https URL with a host and no credentials")
	}
	return nil
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// newInput is the stage input for a validated request.
func newInput(id, database string, c Caller, r Request,
	cfg config.ChangeReviewConfig) Input {
	in := Input{ReviewID: id, Database: database, Actor: c.Actor, Diff: r.Diff, PR: r.PR,
		MigrationTool: r.MigrationTool, Config: cfg}
	if strings.TrimSpace(r.SQL) != "" {
		in.SQLFiles = append(in.SQLFiles, SQLFile{Content: r.SQL})
	}
	in.SQLFiles = append(in.SQLFiles, r.SQLFiles...)
	if r.Transactional != nil {
		in.Transactional, in.TransactionalFrom = *r.Transactional, TransactionalFromRequest
	} else {
		tool, _ := LookupMigrationTool(r.MigrationTool)
		in.Transactional, in.TransactionalFrom = tool.Transactional, TransactionalFromTool
	}
	return in
}
