// Package upgradecheck is the major-version upgrade readiness case
// (market-05): read-only catalog checks that say, for one target major and
// upgrade method, what will stop the upgrade (block), what needs work
// around it (warn) and what pg_sage cannot tell from here (unknown). It
// never writes, never runs DDL and never guesses a target-side fact: a
// fact about the target is read from a fleet member already at that major
// or reported unknown.
package upgradecheck

import (
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/pgversion"
)

// Tag marks every statement the checks run, so they are recognised as
// pg_sage's own.
const Tag = "/* pg_sage upgrade_readiness */ "

// Category is the sage.findings category of a check that does not pass.
const Category = "upgrade_readiness"

// MaxRows caps the evidence rows of one check; Total still counts all.
const MaxRows = 50

// DefaultStatementTimeout bounds each statement of a run.
const DefaultStatementTimeout = 5 * time.Second

// Status is one check's verdict.
type Status string

// The verdicts, from best to worst.
const (
	Pass    Status = "pass"
	Unknown Status = "unknown"
	Warn    Status = "warn"
	Block   Status = "block"
)

// rank orders the verdicts for "the worst of".
func (s Status) rank() int {
	switch s {
	case Block:
		return 3
	case Warn:
		return 2
	case Unknown:
		return 1
	}
	return 0
}

func worst(a, b Status) Status {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// Upgrade methods.
const (
	PGUpgrade = "pg_upgrade" // in place (also what managed major upgrades run)
	BlueGreen = "blue_green" // a provider blue/green deployment (logical)
	Logical   = "logical"    // logical replication into a new cluster
)

var (
	// ErrNoPool is a run asked of no connection pool.
	ErrNoPool = errors.New("upgradecheck: no database connection pool")
	// ErrInvalidTarget is a target pg_sage cannot check.
	ErrInvalidTarget = errors.New("invalid upgrade target")
)

// Target is the upgrade being planned.
type Target struct {
	Major int `json:"major"`
	// Method is PGUpgrade, BlueGreen or Logical; "" is PGUpgrade.
	Method string `json:"method"`
	// Provider is the cloud_environment value ("self-managed", "rds", ...);
	// it selects the dated support table of U7.
	Provider string `json:"provider"`
}

// Normalized is t with the default method.
func (t Target) Normalized() Target {
	if t.Method == "" {
		t.Method = PGUpgrade
	}
	return t
}

// ValidateStatic checks what needs no server: a known method and a major
// pg_sage has dated facts for.
func (t Target) ValidateStatic() error {
	switch t.Normalized().Method {
	case PGUpgrade, BlueGreen, Logical:
	default:
		return fmt.Errorf("%w: method %q is not one of %s, %s, %s", ErrInvalidTarget,
			t.Method, PGUpgrade, BlueGreen, Logical)
	}
	if newest := pgversion.NewestKnownMajor(); t.Major > newest {
		return fmt.Errorf("%w: target_major %d is newer than pg_sage knows (up to %d)",
			ErrInvalidTarget, t.Major, newest)
	}
	if t.Major <= 0 {
		return fmt.Errorf("%w: target_major must be a positive major such as %d",
			ErrInvalidTarget, pgversion.NewestKnownMajor())
	}
	return nil
}

// Validate checks t against the source server's major.
func (t Target) Validate(sourceMajor int) error {
	if err := t.ValidateStatic(); err != nil {
		return err
	}
	if t.Major <= sourceMajor {
		return fmt.Errorf("%w: target_major %d must be above the server's major %d",
			ErrInvalidTarget, t.Major, sourceMajor)
	}
	return nil
}

// Row is one evidence row, column name to text.
type Row map[string]string

// Check is one check's verdict with its evidence.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Reason string `json:"reason"`
	// Rows are at most MaxRows; Total counts every matching row.
	Rows  []Row `json:"rows"`
	Total int   `json:"total"`
	// SQLState is the error code of a check that could not read.
	SQLState string `json:"sqlstate,omitempty"`
	// Skipped names the arms that do not apply here, with why.
	Skipped []string `json:"skipped,omitempty"`
}

// Report is one run's checks, in U1..U7 order.
type Report struct {
	Database         string    `json:"database"`
	SourceVersionNum int       `json:"source_version_num"`
	SourceMajor      int       `json:"source_major"`
	Target           Target    `json:"target"`
	TargetDatabase   string    `json:"target_database,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
	Checks           []Check   `json:"checks"`
}

// Check returns the check with id.
func (r Report) Check(id string) (Check, bool) {
	for _, c := range r.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return Check{}, false
}
