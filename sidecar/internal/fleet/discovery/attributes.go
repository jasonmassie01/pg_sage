package discovery

import (
	"fmt"
	"regexp"
	"strings"
)

// Candidate attributes (fleetpol-01b): tags and a creator, which policy
// set selectors match on. A value that cannot be trusted is dropped with
// a reason, never stored: a malformed creator must not make a database
// look agent-created, or not.

// commentPrefix starts a cluster database comment pg_sage reads:
// COMMENT ON DATABASE app IS 'pg_sage: creator=agent:builder owner=team'.
const commentPrefix = "pg_sage:"

// Attribute bounds.
const (
	maxTagKey      = 64
	maxTagValue    = 256
	maxCreator     = 256
	creatorTag     = "creator"
	shownKeyPrefix = 20
)

// tagKeyPattern is the policy selector's tag key alphabet.
var tagKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// parseDatabaseComment reads a pg_sage comment: whitespace-separated
// key=value pairs after "pg_sage:"; creator is the creator, every other
// key a tag. Any other comment carries nothing.
func parseDatabaseComment(comment string) (creator string, tags map[string]string,
	problems []string) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(comment), commentPrefix)
	if !ok {
		return "", nil, nil
	}
	tags = map[string]string{}
	for _, field := range strings.Fields(rest) {
		key, value, ok := strings.Cut(field, "=")
		switch {
		case !ok || key == "":
			problems = append(problems, fmt.Sprintf("comment item %q is not key=value; "+
				"ignored", shorten(field)))
		case key == creatorTag && value == "":
			problems = append(problems, "comment creator is empty; ignored, so the "+
				"database is not treated as agent-created")
		case key == creatorTag:
			creator = value
		default:
			if p := tagProblem(key, value); p != "" {
				problems = append(problems, p)
				continue
			}
			tags[key] = value
		}
	}
	return creator, tags, problems
}

// cleanAttributes drops the tags and creator of c that a selector cannot
// trust and returns why. A missing creator is taken from the "creator"
// tag (RDS); the source's own ignored items are kept first. c's maps are
// replaced, never changed: a source may hand out the same map each cycle.
func cleanAttributes(c *Candidate) []string {
	problems := append([]string(nil), c.Ignored...)
	tags := make(map[string]string, len(c.Tags))
	for key, value := range c.Tags {
		if p := tagProblem(key, value); p != "" {
			problems = append(problems, p)
			continue
		}
		tags[key] = value
	}
	c.Tags = tags
	c.Creator = strings.TrimSpace(c.Creator)
	if c.Creator == "" {
		c.Creator = strings.TrimSpace(c.Tags[creatorTag])
	}
	if c.Creator != "" && (len(c.Creator) > maxCreator ||
		strings.ContainsFunc(c.Creator, isSpace)) {
		problems = append(problems, fmt.Sprintf("creator %q is longer than %d or has "+
			"spaces; ignored", shorten(c.Creator), maxCreator))
		c.Creator = ""
	}
	c.Ignored = nil
	return problems
}

func tagProblem(key, value string) string {
	switch {
	case len(key) > maxTagKey:
		return fmt.Sprintf("tag key %q is longer than %d characters; ignored",
			shorten(key), maxTagKey)
	case !tagKeyPattern.MatchString(key):
		return fmt.Sprintf("tag key %q has characters outside A-Z a-z 0-9 _ . / -; ignored",
			shorten(key))
	case len(value) > maxTagValue:
		return fmt.Sprintf("tag %q value is longer than %d characters; ignored", key,
			maxTagValue)
	}
	return ""
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func shorten(s string) string {
	if len(s) <= shownKeyPrefix {
		return s
	}
	return s[:shownKeyPrefix] + "..."
}
