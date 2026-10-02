// Package pooler is Sage SRE's optional, read-only telemetry probe for an
// external connection pooler (CHECK-04): PgBouncer's admin console read
// with SHOW POOLS and SHOW STATS only, bounded in time and rows, as the
// pooler_pools signal probe of a connection investigation. Credentials
// come from configuration (environment or a mounted secret) and never
// appear in a result, an error or a log line.
package pooler

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The admin console commands the probe may run; nothing else is sent.
const (
	ShowPools = "POOLS"
	ShowStats = "STATS"
)

// Failure reasons (never the underlying error text, which can carry the
// host, user or DSN).
const (
	ReasonUnreachable = "pooler_unreachable"
	ReasonAuthFailed  = "pooler_auth_failed"
	ReasonTimeout     = "pooler_timeout"
	ReasonProtocol    = "pooler_protocol_error"
)

// Version is the pooler_pools result version.
const Version = "v1"

// Bounds of one pooler's read and of a configuration.
const (
	MinTimeout = 50 * time.Millisecond
	MaxTimeout = 2 * time.Second
	maxPools   = 100
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// Config is one pooler: a name, its admin-console DSN (a secret), the
// PgBouncer databases to report (empty: every pool) and its timeout.
type Config struct {
	Name    string
	DSN     string
	Pools   []string
	Timeout time.Duration
}

// Admin is a read-only admin-console session.
type Admin interface {
	// Show runs SHOW <what> (ShowPools or ShowStats) and returns its rows
	// by column name.
	Show(ctx context.Context, what string) ([]map[string]any, error)
	Close(ctx context.Context) error
}

// Dialer opens an admin-console session.
type Dialer func(ctx context.Context, dsn string) (Admin, error)

// Source reads every configured pooler of one database.
type Source struct {
	poolers []Config
	pools   []map[string]bool
	dial    Dialer
}

// New validates the poolers. Errors name the pooler, never its DSN.
func New(cfgs []Config, dial Dialer) (*Source, error) {
	if dial == nil {
		return nil, errors.New("pooler telemetry needs a dialer")
	}
	if len(cfgs) == 0 {
		return nil, errors.New("pooler telemetry needs at least one pooler")
	}
	s := &Source{dial: dial}
	seen := map[string]bool{}
	for i, c := range cfgs {
		if err := validate(c, seen); err != nil {
			return nil, fmt.Errorf("pooler %d: %w", i+1, err)
		}
		filter := map[string]bool{}
		for _, p := range c.Pools {
			filter[p] = true
		}
		s.poolers = append(s.poolers, c)
		s.pools = append(s.pools, filter)
	}
	return s, nil
}

func validate(c Config, seen map[string]bool) error {
	switch {
	case !namePattern.MatchString(c.Name):
		return errors.New("name must be 1-63 letters, digits, '.', '_' or '-'")
	case seen[c.Name]:
		return fmt.Errorf("duplicate name %q", c.Name)
	case c.DSN == "":
		return fmt.Errorf("%q has no DSN", c.Name)
	case c.Timeout < MinTimeout || c.Timeout > MaxTimeout:
		return fmt.Errorf("%q timeout %s outside [%s, %s]", c.Name, c.Timeout, MinTimeout,
			MaxTimeout)
	case len(c.Pools) > maxPools:
		return fmt.Errorf("%q lists %d pools, at most %d", c.Name, len(c.Pools), maxPools)
	}
	seen[c.Name] = true
	return nil
}

// Probe reads every pooler and returns one pooler_pools result: a row
// per pool and a row per pooler that could not be read. It fails only
// when no pooler could be read.
func (s *Source) Probe(ctx context.Context, args probes.Args) probes.Result {
	res := probes.Result{ProbeID: probes.PoolerPools, Version: Version,
		ObservedAt: time.Now()}
	switch {
	case s == nil:
		return failed(res, probes.StatusError, "not_configured")
	case args != (probes.Args{}):
		return failed(res, probes.StatusError, "invalid_args")
	case ctx.Err() != nil:
		return failed(res, probes.StatusError, "canceled")
	}
	start := time.Now()
	var firstFail *readError
	read := 0
	for i, c := range s.poolers {
		rows, err := s.read(ctx, c, s.pools[i])
		if err != nil {
			if firstFail == nil {
				firstFail = err
			}
			res.Rows = append(res.Rows, probes.Row{"pooler": c.Name, "unreachable": true,
				"reason": err.reason})
			continue
		}
		read++
		res.Rows = append(res.Rows, rows...)
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	if read == 0 {
		return failed(res, firstFail.status, firstFail.reason)
	}
	return finish(res)
}

// finish caps the rows, names the columns and sets the status.
func finish(res probes.Result) probes.Result {
	if len(res.Rows) > probes.MaxRows {
		res.Rows, res.Truncated = res.Rows[:probes.MaxRows], true
	}
	res.Columns = []string{"pooler", "database", "user", "pool_mode", "cl_active",
		"cl_waiting", "sv_active", "sv_idle", "sv_used", "maxwait_s", "avg_wait_us",
		"unreachable", "reason"}
	res.Status = probes.StatusOK
	if len(res.Rows) == 0 {
		res.Status, res.Reason = probes.StatusEmpty, "no_rows"
	}
	return res
}

func failed(res probes.Result, st probes.Status, reason string) probes.Result {
	res.Status, res.Reason, res.Rows, res.Truncated = st, reason, nil, false
	return res
}
