// Package changefeed is the Sage SRE change feed (AI-SRE-SPEC §4 R1 change
// feed v0 and R1.1 signed change events): one durable timeline of what
// changed around a database. External systems submit signed deploys,
// migrations, feature flags and config changes; pg_sage's own sources
// (its actions, sage.config_audit, DDL seen by the migration detector,
// pg_stat_statements resets, restarts, failovers and extension version
// changes) are polled. Investigations read the feed in their window as
// typed change_feed evidence, so "what changed?" is answered with
// evidence. Event text is untrusted data: bounded, never control
// characters, never SQL text or setting values.
package changefeed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Kind classifies a change.
type Kind string

// Change kinds. External systems may submit deploy, migration,
// feature_flag, config and other; the rest come only from pg_sage's own
// sources, so they cannot be forged through ingestion.
const (
	KindDeploy      Kind = "deploy"
	KindMigration   Kind = "migration"
	KindFeatureFlag Kind = "feature_flag"
	KindConfig      Kind = "config"
	KindDDL         Kind = "ddl"
	KindSageAction  Kind = "sage_action"
	KindStatsReset  Kind = "stats_reset"
	KindRestart     Kind = "restart"
	KindFailover    Kind = "failover"
	KindExtension   Kind = "extension"
	KindOther       Kind = "other"
)

// External reports whether external systems may submit the kind.
func (k Kind) External() bool {
	switch k {
	case KindDeploy, KindMigration, KindFeatureFlag, KindConfig, KindOther:
		return true
	}
	return false
}

// Signature statuses and pg_sage's own source name.
const (
	SignatureVerified = "verified"
	SignatureInternal = "internal"
	SourceSage        = "pg_sage"
)

// Submission bounds.
const (
	maxEventIDRunes = 128
	maxSummaryRunes = 300
	maxServiceRunes = 128
	maxLinkBytes    = 512
	maxObjects      = 20
	maxObjectRunes  = 128
	maxPast         = 7 * 24 * time.Hour
	maxFuture       = 5 * time.Minute
)

// Errors; callers distinguish them with errors.Is.
var (
	ErrInvalid  = errors.New("invalid change event")
	ErrConflict = errors.New("change event id already recorded with other content")
)

var sourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ValidSource reports whether s is a well-formed source name.
func ValidSource(s string) bool { return sourcePattern.MatchString(s) }

// Event is one change of the feed.
type Event struct {
	ID         sre.UUID  `json:"id"`
	Source     string    `json:"source"`
	EventID    string    `json:"event_id"`
	Kind       Kind      `json:"kind"`
	Scoped     bool      `json:"database_scoped"`
	Service    string    `json:"service,omitempty"`
	Summary    string    `json:"summary"`
	Link       string    `json:"link,omitempty"`
	Objects    []string  `json:"objects"`
	OccurredAt time.Time `json:"occurred_at"`
	ReceivedAt time.Time `json:"received_at"`
	Signature  string    `json:"signature"`
	Hash       string    `json:"hash"`
}

// Submission is the signed JSON body of POST /api/v1/sre/change-events.
// An empty Database is a deployment-wide change.
type Submission struct {
	Source     string    `json:"source"`
	EventID    string    `json:"event_id"`
	Kind       Kind      `json:"kind"`
	Database   string    `json:"database,omitempty"`
	Service    string    `json:"service,omitempty"`
	Summary    string    `json:"summary"`
	Link       string    `json:"link,omitempty"`
	Objects    []string  `json:"objects,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Event validates the submission and returns the verified event it
// describes, received at now.
func (s Submission) Event(now time.Time) (Event, error) {
	if err := s.validate(now); err != nil {
		return Event{}, err
	}
	e := Event{Source: s.Source, EventID: s.EventID, Kind: s.Kind, Service: s.Service,
		Summary: s.Summary, Link: s.Link, Objects: append([]string{}, s.Objects...),
		OccurredAt: s.OccurredAt.UTC(), ReceivedAt: now.UTC(),
		Signature: SignatureVerified, Scoped: s.Database != ""}
	e.Hash = hashOf(e)
	return e, nil
}

func (s Submission) validate(now time.Time) error {
	switch {
	case !ValidSource(s.Source):
		return invalid("source must match %s", sourcePattern)
	case !s.Kind.External():
		return invalid("kind %q cannot be submitted", s.Kind)
	case s.OccurredAt.IsZero():
		return invalid("occurred_at is required")
	case s.OccurredAt.Before(now.Add(-maxPast)) || s.OccurredAt.After(now.Add(maxFuture)):
		return invalid("occurred_at must be within the last 7 days")
	}
	checks := []struct {
		name, value string
		required    bool
		max         int
	}{{"event_id", s.EventID, true, maxEventIDRunes},
		{"summary", s.Summary, true, maxSummaryRunes},
		{"service", s.Service, false, maxServiceRunes}}
	for _, c := range checks {
		if err := checkText(c.name, c.value, c.required, c.max); err != nil {
			return err
		}
	}
	if err := checkLink(s.Link); err != nil {
		return err
	}
	return checkObjects(s.Objects)
}

func checkText(name, value string, required bool, max int) error {
	switch {
	case value == "" && required:
		return invalid("%s is required", name)
	case utf8.RuneCountInString(value) > max:
		return invalid("%s is longer than %d characters", name, max)
	case !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0:
		return invalid("%s contains control characters", name)
	}
	return nil
}

func checkLink(link string) error {
	if link == "" {
		return nil
	}
	if len(link) > maxLinkBytes {
		return invalid("link is longer than %d bytes", maxLinkBytes)
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return invalid("link must be an http(s) URL")
	}
	if u.User != nil {
		return invalid("link must not carry credentials")
	}
	return checkText("link", link, false, maxLinkBytes)
}

func checkObjects(objects []string) error {
	if len(objects) > maxObjects {
		return invalid("at most %d objects", maxObjects)
	}
	for _, o := range objects {
		if err := checkText("object", o, true, maxObjectRunes); err != nil {
			return err
		}
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// hashOf is the SHA-256 of an event's content (not its receive time or
// row id), hex encoded: a replay hashes the same, a changed event does not.
func hashOf(e Event) string {
	parts := []string{e.Source, e.EventID, string(e.Kind), e.Service, e.Summary, e.Link,
		strings.Join(e.Objects, "\x1f"), e.OccurredAt.UTC().Format(time.RFC3339Nano),
		fmt.Sprint(e.Scoped)}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// internalEvent is a change from one of pg_sage's own sources.
func internalEvent(kind Kind, eventID, summary string, at, now time.Time) Event {
	e := Event{Source: SourceSage, EventID: truncate(eventID, maxEventIDRunes), Kind: kind,
		Summary: cleanSummary(summary), Objects: []string{}, OccurredAt: at.UTC(),
		ReceivedAt: now.UTC(), Signature: SignatureInternal, Scoped: true}
	e.Hash = hashOf(e)
	return e
}

// cleanSummary makes text read from pg_sage's tables safe to store:
// control characters become spaces and it is bounded.
func cleanSummary(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	s = truncate(strings.TrimSpace(s), maxSummaryRunes)
	if s == "" {
		return "(no description)"
	}
	return s
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
