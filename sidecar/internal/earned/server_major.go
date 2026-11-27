package earned

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// earned-07: a level records the PostgreSQL major of the monitored server
// it was granted on. Planner, statistics and GUC semantics change between
// majors, so a self-initiated L3 earned on one major does not run
// unattended on the next until it re-validates (major_cap.go).

// MajorSource reads the monitored server's PostgreSQL major version.
type MajorSource func(ctx context.Context) (int, error)

// MajorProbeTTL is how long a probe trusts the major it last read.
const MajorProbeTTL = 5 * time.Minute

// majorProbeTimeout bounds one read of server_version_num.
const majorProbeTimeout = 5 * time.Second

// ParseServerVersionNum is the major of a server_version_num setting
// (170002 is 17). Only PostgreSQL 10 or later (six digits) is accepted.
func ParseServerVersionNum(setting string) (int, error) {
	s := strings.TrimSpace(setting)
	n, err := strconv.Atoi(s)
	if err != nil || len(s) < 6 || len(s) > 7 || n < 100000 {
		return 0, fmt.Errorf("server_version_num %q is not a PostgreSQL 10+ version number",
			truncate(setting, 40))
	}
	return n / 10000, nil
}

// majorQuerier is the read the probe makes.
type majorQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MajorProbe reads server_version_num from the monitored database, once
// per ttl, with one bounded read-only statement (the setting is readable
// without privileges on every provider). A failed read keeps the last
// known major and logs; with none known it is an error.
type MajorProbe struct {
	q    majorQuerier
	ttl  time.Duration
	now  func() time.Time
	logf func(string, ...any)

	mu     sync.Mutex
	major  int
	readAt time.Time
}

// NewMajorProbe probes pool (the monitored database); logf may be nil.
func NewMajorProbe(pool *pgxpool.Pool, logf func(string, ...any)) *MajorProbe {
	var q majorQuerier
	if pool != nil {
		q = pool
	}
	return newMajorProbe(q, MajorProbeTTL, time.Now, logf)
}

func newMajorProbe(q majorQuerier, ttl time.Duration, now func() time.Time,
	logf func(string, ...any)) *MajorProbe {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &MajorProbe{q: q, ttl: ttl, now: now, logf: logf}
}

// Major is the server's major: the cached one inside the ttl, else a
// fresh read (MajorSource).
func (p *MajorProbe) Major(ctx context.Context) (int, error) {
	if p == nil || p.q == nil {
		return 0, errors.New("server major: no monitored database to probe")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.major > 0 && now.Sub(p.readAt) < p.ttl {
		return p.major, nil
	}
	major, err := p.read(ctx)
	if err != nil {
		if p.major > 0 {
			p.logf("autonomy: %v; keeping the last known PostgreSQL major %d", err, p.major)
			return p.major, nil
		}
		p.logf("autonomy: %v; no PostgreSQL major known, levels are not re-validated", err)
		return 0, err
	}
	p.major, p.readAt = major, now
	return major, nil
}

func (p *MajorProbe) read(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, majorProbeTimeout)
	defer cancel()
	var setting string
	if err := p.q.QueryRow(ctx, `/* pg_sage */ SELECT
		current_setting('server_version_num')`).Scan(&setting); err != nil {
		return 0, fmt.Errorf("read server_version_num: %w", err)
	}
	return ParseServerVersionNum(setting)
}
