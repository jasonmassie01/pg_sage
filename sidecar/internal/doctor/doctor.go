// Package doctor is pg_sage's preflight (ux-04a): a fixed list of read-only
// checks a person runs before or after install, each with a status, what
// it found and a copyable fix. Doctor executes nothing it suggests: every
// Fix is a statement or setting for the operator to apply.
package doctor

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Status is one check's outcome.
type Status string

const (
	// StatusPass: the prerequisite is met.
	StatusPass Status = "pass"
	// StatusWarn: pg_sage runs, with less than it could do.
	StatusWarn Status = "warn"
	// StatusFail: a prerequisite is missing; the exit code is 1.
	StatusFail Status = "fail"
	// StatusUnknown: the check could not run or was not asked for.
	StatusUnknown Status = "unknown"
)

// Check IDs, in report order. The order and the IDs are a contract with
// the CLI's JSON output and the setup wizard (ux-04b).
const (
	IDConnection   = "connection"
	IDVersion      = "postgres_version"
	IDRoleGrants   = "role_grants"
	IDExtensions   = "extensions"
	IDProvider     = "provider"
	IDQueryText    = "query_text"
	IDModel        = "model"
	IDNotification = "notification"
	IDMetaDB       = "meta_db"
	IDClockSkew    = "clock_skew"
)

// detailNoConnection is every database check's detail after the
// connection check failed: one failure, not a cascade of them.
const detailNoConnection = "no connection: fix the connection check first"

// Check is one preflight result. Fix is empty when there is nothing to do.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// Counts is how many checks ended in each status.
type Counts struct {
	Pass    int `json:"pass"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
	Unknown int `json:"unknown"`
}

// Report is one doctor run. Provider is the platform code (SM, RDS, AUR,
// GCP, AZ, NEON, SUPA, LB) or "unknown".
type Report struct {
	Provider string  `json:"provider"`
	Checks   []Check `json:"checks"`
	Counts   Counts  `json:"counts"`
}

// ModelProbe sends the one tiny model request POST /api/v1/llm/test sends
// (llm.Client.Probe).
type ModelProbe func(ctx context.Context) llm.ProbeResult

// NotifyTest sends one test notification through every configured channel
// and returns how many it sent through.
type NotifyTest func(ctx context.Context) (int, error)

// Inputs is what one run checks. A nil Target fails the connection check;
// a nil Model, Notify or Meta (with no MetaErr) leaves that check unknown.
type Inputs struct {
	// Target is the monitored database. Checks run on it one at a time.
	Target *pgxpool.Pool
	// Model probes the configured model; nil when none is configured.
	Model ModelProbe
	// ModelSkipped, when set, is why the model is not probed this run.
	ModelSkipped string
	// Notify sends the test notification; nil unless the operator asked.
	Notify NotifyTest
	// Meta is the meta database; MetaErr why it could not be opened.
	Meta    *pgxpool.Pool
	MetaErr error
	// ExpectedSteps are the migration steps this build applies
	// (schema.MigrationStepNames); empty skips the comparison.
	ExpectedSteps []string
	// Now is the host clock (a seam for the skew check); nil is time.Now.
	Now func() time.Time
	// Secrets are masked wherever they appear in a detail or fix.
	Secrets []string
}

// Count tallies the checks by status.
func Count(checks []Check) Counts {
	var c Counts
	for _, ch := range checks {
		switch ch.Status {
		case StatusPass:
			c.Pass++
		case StatusWarn:
			c.Warn++
		case StatusFail:
			c.Fail++
		default:
			c.Unknown++
		}
	}
	return c
}

// ExitCode is the CLI's exit code for the report: 1 when any check
// failed, else 0.
func (r Report) ExitCode() int {
	if r.Counts.Fail > 0 {
		return 1
	}
	return 0
}

// Find returns the check with id.
func (r Report) Find(id string) (Check, bool) {
	for _, c := range r.Checks {
		if c.ID == id {
			return c, true
		}
	}
	return Check{}, false
}

// Run runs every check in order, sequentially, and never panics on nil
// inputs. Every detail and fix is masked for passwords before it returns.
func Run(ctx context.Context, in Inputs) Report {
	if in.Now == nil {
		in.Now = time.Now
	}
	db := runDatabaseChecks(ctx, in)
	checks := []Check{db.connection, db.version, db.role, db.extensions,
		db.provider, db.queryText,
		modelCheck(ctx, in.Model, in.ModelSkipped),
		notifyCheck(ctx, in.Notify),
		metaCheck(ctx, in.Meta, in.MetaErr, in.ExpectedSteps),
		db.clock,
	}
	for i := range checks {
		checks[i].Detail = mask(checks[i].Detail, in.Secrets)
		checks[i].Fix = mask(checks[i].Fix, in.Secrets)
	}
	return Report{Provider: db.platform.Code, Checks: checks, Counts: Count(checks)}
}

// unknownCheck is a check that could not run.
func unknownCheck(id, title, detail string) Check {
	return Check{ID: id, Title: title, Status: StatusUnknown, Detail: detail}
}
