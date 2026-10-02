package pooler

import (
	"context"
	"errors"
	"math"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// readError is a classified failure to read one pooler.
type readError struct {
	status probes.Status
	reason string
}

// read connects to one pooler within its timeout and returns its pools.
// SHOW STATS is best effort: without it the wait averages are unknown.
func (s *Source) read(ctx context.Context, c Config, filter map[string]bool) ([]probes.Row,
	*readError) {
	rctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	admin, err := s.dial(rctx, c.DSN)
	if err != nil {
		return nil, classify(rctx, err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	pools, err := admin.Show(rctx, ShowPools)
	if err != nil {
		return nil, classify(rctx, err)
	}
	waits := map[string]float64{}
	if stats, err := admin.Show(rctx, ShowStats); err == nil {
		for _, st := range stats {
			if db, ok := st["database"].(string); ok {
				waits[db] = number(st["avg_wait_time"])
			}
		}
	}
	return poolRows(c.Name, pools, filter, waits)
}

// poolRows turns SHOW POOLS rows into pooler_pools rows, leaving out the
// admin console and the pools the configuration does not name.
func poolRows(name string, pools []map[string]any, filter map[string]bool,
	waits map[string]float64) ([]probes.Row, *readError) {
	var out []probes.Row
	for _, p := range pools {
		db, ok := p["database"].(string)
		if !ok {
			return nil, &readError{probes.StatusError, ReasonProtocol}
		}
		if db == probes.PoolerAdminDatabase || (len(filter) > 0 && !filter[db]) {
			continue
		}
		user, _ := p["user"].(string)
		mode, _ := p["pool_mode"].(string)
		wait, ok := waits[db]
		if !ok {
			wait = math.NaN()
		}
		out = append(out, probes.Row{"pooler": name, "database": db, "user": user,
			"pool_mode": mode, "cl_active": field(p, "cl_active"),
			"cl_waiting": field(p, "cl_waiting"), "sv_active": field(p, "sv_active"),
			"sv_idle": field(p, "sv_idle"), "sv_used": field(p, "sv_used"),
			"maxwait_s": maxWait(p), "avg_wait_us": known(wait)})
		if len(out) > probes.MaxRows {
			break
		}
	}
	return out, nil
}

// maxWait is the oldest client's wait in seconds: maxwait plus, since
// PgBouncer 1.8, its microseconds part.
func maxWait(p map[string]any) any {
	s := number(p["maxwait"])
	if math.IsNaN(s) {
		return nil
	}
	if us := number(p["maxwait_us"]); !math.IsNaN(us) {
		s += us / 1e6
	}
	return s
}

// field is one numeric column, nil when unknown.
func field(p map[string]any, key string) any { return known(number(p[key])) }

// number reads an admin-console value; unknown is NaN.
func number(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case int:
		return float64(x)
	case float64:
		return x
	}
	return math.NaN()
}

// known is a JSON-safe number: nil for unknown (stored evidence cannot
// hold NaN; a missing value decodes as unknown).
func known(v float64) any {
	if math.IsNaN(v) {
		return nil
	}
	return v
}

// classify maps a failure to a status and reason without its text.
func classify(ctx context.Context, err error) *readError {
	var pgErr *pgconn.PgError
	var netErr net.Error
	switch {
	case errors.As(err, &pgErr) && authFailure(pgErr):
		return &readError{probes.StatusNoPrivilege, ReasonAuthFailed}
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil ||
		(errors.As(err, &netErr) && netErr.Timeout()):
		return &readError{probes.StatusError, ReasonTimeout}
	case errors.As(err, &pgErr):
		return &readError{probes.StatusError, ReasonProtocol}
	}
	return &readError{probes.StatusError, ReasonUnreachable}
}

// authFailure recognizes a refused login or admin access. PostgreSQL uses
// SQLSTATE class 28; PgBouncer reports most client errors as 08P01, so
// its own login and console refusals are recognized by their text.
func authFailure(e *pgconn.PgError) bool {
	if strings.HasPrefix(e.Code, "28") {
		return true
	}
	msg := strings.ToLower(e.Message)
	for _, s := range []string{"authentication failed", "not allowed", "admin access",
		"no such user"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
