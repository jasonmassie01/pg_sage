// Package agentposture is pg_sage's agent posture check (G0, spec §6.15):
// deterministic, catalog-only detectors (AP-01..AP-16) that report how
// exposed a database is to the agents and untrusted clients that touch
// it: over-privileged agent roles, tables reachable without row-level
// security, definer functions without a pinned search_path and so on.
//
// Detectors run in the first look's "Agent posture" section and, through
// Monitor, in the analyzer when the catalog they read changes and daily.
// They only read the catalog, in a read-only transaction, and they never
// act: every fix is a manual script for the operator (L1, G0-05).
//
// A detector is one type implementing Detector, registered from an init
// function with Register. The registry, the runner and the conversions to
// first-look items and analyzer findings are shared by every detector.
package agentposture

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Severity of a posture finding.
type Severity string

// Severities, lowest first.
const (
	Info     Severity = "info"
	Warning  Severity = "warning"
	Critical Severity = "critical"
)

var severityRank = map[Severity]int{Info: 1, Warning: 2, Critical: 3}

// Valid reports whether s is one of the three severities.
func (s Severity) Valid() bool { return severityRank[s] > 0 }

// AtMost reports whether s is valid and no higher than max.
func (s Severity) AtMost(max Severity) bool {
	return s.Valid() && severityRank[s] <= severityRank[max]
}

// ProposalLevel is the highest autonomy level any posture finding
// proposes: L1, a manual script the operator runs (G0-05). A posture
// finding carries no executable SQL, so the executor never acts on it.
const ProposalLevel = "L1"

// Section names the first-look section and the finding detail section.
const Section = "agent_posture"

// CategoryPrefix starts every posture finding category ("agent_posture:AP-03").
const CategoryPrefix = Section + ":"

// Tag starts every posture statement: posture runs as part of the first
// look, so the first look's performance gate covers it (G0-04).
const Tag = "/* pg_sage first_look */ "

// Statement tags one detector statement: the first-look tag, then the
// detector id, so pg_stat_statements attributes its cost.
func Statement(detectorID, sql string) string {
	return Tag + "/* " + Section + " " + detectorID + " */ " + sql
}

// Errors, each distinguishable with errors.Is.
var (
	ErrNoPool          = errors.New("agentposture: no database connection pool")
	ErrInvalidDetector = errors.New("agentposture: invalid detector")
	ErrInvalidFinding  = errors.New("agentposture: invalid finding")
	ErrInvalidConfig   = errors.New("agentposture: invalid configuration")
)

// Evidence is one catalog observation a finding rests on.
type Evidence struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
	Detail string `json:"detail,omitempty"`
}

// Finding is one posture problem on one object. FixScript is a manual
// script for the operator to review and run; pg_sage never runs it.
type Finding struct {
	// Detector is the detector id (AP-01); the runner fills it in.
	Detector string
	Severity Severity
	// ObjectType and Object identify the object: "role" and its name,
	// "table" and schema.name, "function" and schema.name(args), ...
	// Category and Object together are the finding's identity.
	ObjectType     string
	Object         string
	Title          string
	Detail         string
	Recommendation string
	FixScript      string
	Caveat         string
	Evidence       []Evidence
}

// Category is the finding's category in sage.findings.
func (f Finding) Category() string { return CategoryPrefix + f.Detector }

// IsCategory reports whether a finding category is a posture category.
func IsCategory(category string) bool {
	return strings.HasPrefix(category, CategoryPrefix) && len(category) > len(CategoryPrefix)
}

// Arm is a version-specific part of a detector. Outside its server
// version range the arm is skipped, and the skip is recorded with
// SkipReason in the check's note (G0-03).
type Arm struct {
	Name string
	// MinVersion is the lowest server_version_num the arm runs on
	// (inclusive); 0 is no lower bound.
	MinVersion int
	// MaxVersion is the first server_version_num the arm no longer runs
	// on (exclusive); 0 is no upper bound.
	MaxVersion int
	SkipReason string
}

// Applies reports whether the arm runs on server version v. An unknown
// version (0) satisfies only an unbounded arm.
func (a Arm) Applies(v int) bool {
	if a.MinVersion > 0 && v < a.MinVersion {
		return false
	}
	if a.MaxVersion > 0 && (v == 0 || v >= a.MaxVersion) {
		return false
	}
	return true
}

func (a Arm) bounded() bool { return a.MinVersion > 0 || a.MaxVersion > 0 }

// Spec declares a detector: its id (AP-NN), its check title, the highest
// severity it may report and its version arms.
type Spec struct {
	ID       string
	Title    string
	Severity Severity
	Arms     []Arm
}

// Detector is one posture check.
type Detector interface {
	Spec() Spec
	// Detect reads the catalog through in.Q and returns its findings.
	// Every statement it runs is built with Statement.
	Detect(ctx context.Context, in Input) ([]Finding, error)
}

// Querier is the read-only transaction detectors run in.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Input is what a detector reads: the transaction, the resolved
// environment and which of its arms apply to the server.
type Input struct {
	Q      Querier
	Env    Env
	active map[string]bool
}

// Arm reports whether the detector's arm name applies to this server.
func (in Input) Arm(name string) bool { return in.active[name] }

// QuoteIdent quotes a PostgreSQL identifier unless it is a plain
// lower-case name, so fix scripts stay readable and safe to paste.
func QuoteIdent(name string) string {
	bare := name != "" && (name[0] < '0' || name[0] > '9')
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			bare = false
			break
		}
	}
	if bare && !reservedWords[name] {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QualifiedName is schema.name, each part quoted when needed.
func QualifiedName(schema, name string) string {
	return QuoteIdent(schema) + "." + QuoteIdent(name)
}

// reservedWords are the reserved key words most likely as object names.
var reservedWords = map[string]bool{"user": true, "order": true, "group": true,
	"table": true, "select": true, "from": true, "where": true, "default": true,
	"check": true, "column": true, "limit": true, "offset": true, "end": true,
	"primary": true, "references": true, "all": true, "analyse": true, "analyze": true,
	"grant": true, "to": true}
