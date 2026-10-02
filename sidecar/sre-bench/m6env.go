package srebench

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Fixture helpers of the M6 fault programs: looping sessions, cluster
// settings changed through ALTER SYSTEM (and always restored), counter
// reads that differ by PostgreSQL version, and a sampler that notices
// other databases' load during a run.

// loop runs sql on a new tracked session until the session ends (the
// harness terminates it); setup runs first. pace, when positive, is the
// least time one iteration takes. The statement that ended the loop is
// the session's outcome, which the safety grader reads.
func (e *Env) loop(ctx context.Context, app, setup, sql string,
	pace time.Duration) (int, error) {
	s, err := e.connect(ctx, app)
	if err != nil {
		return 0, err
	}
	if setup != "" {
		if _, err := s.conn.PgConn().Exec(ctx, setup).ReadAll(); err != nil {
			close(s.done)
			return 0, fmt.Errorf("loop setup: %w", err)
		}
	}
	go func() {
		defer close(s.done)
		for {
			start := time.Now()
			if _, err := s.conn.Exec(context.Background(), sql); err != nil {
				s.err = err
				return
			}
			if d := pace - time.Since(start); d > 0 {
				time.Sleep(d)
			}
		}
	}()
	return s.pid, nil
}

// serverVersion is server_version_num.
func (e *Env) serverVersion(ctx context.Context) (int, error) {
	return e.count(ctx, "SELECT current_setting('server_version_num')::int")
}

// requestedCheckpoints reads the cluster's requested checkpoint counter
// (pg_stat_checkpointer from PostgreSQL 17, pg_stat_bgwriter before).
func (e *Env) requestedCheckpoints(ctx context.Context) (int, error) {
	v, err := e.serverVersion(ctx)
	if err != nil {
		return 0, err
	}
	if v >= 170000 {
		return e.count(ctx, "SELECT num_requested::int FROM pg_catalog.pg_stat_checkpointer")
	}
	return e.count(ctx, "SELECT checkpoints_req::int FROM pg_catalog.pg_stat_bgwriter")
}

// stableCheckpoints waits until the requested checkpoint counter stops
// moving (a just-finished checkpoint's statistics are reported) and
// returns it.
func (e *Env) stableCheckpoints(ctx context.Context) (int, error) {
	prev := -1
	var cur int
	err := waitFor(ctx, "checkpoint counter to settle", func() (bool, error) {
		var err error
		if cur, err = e.requestedCheckpoints(ctx); err != nil {
			return false, err
		}
		settled := cur == prev
		prev = cur
		if !settled {
			time.Sleep(250 * time.Millisecond)
		}
		return settled, nil
	})
	return cur, err
}

// alterSystem sets a cluster setting with ALTER SYSTEM and reloads. A
// setting another ALTER SYSTEM already set is refused: resetting it
// afterwards would not restore it.
func (e *Env) alterSystem(ctx context.Context, name, value string) error {
	n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_settings
		WHERE name = $1 AND sourcefile LIKE '%postgresql.auto.conf'`, name)
	if err != nil {
		return &Unsupported{Reason: "pg_settings not readable: " + err.Error()}
	}
	if n > 0 {
		return &Unsupported{Reason: name + " is already set by ALTER SYSTEM"}
	}
	// ALTER SYSTEM takes no parameters; name and value are code constants,
	// quoted as an identifier and a literal.
	stmt := fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", pgx.Identifier{name}.Sanitize(),
		strings.ReplaceAll(value, "'", "''"))
	if _, err := e.Pool.Exec(ctx, stmt); err != nil {
		return &Unsupported{Reason: "ALTER SYSTEM not permitted: " + err.Error()}
	}
	return e.reloadAndWait(ctx, name, value)
}

// resetSystem removes the ALTER SYSTEM setting and waits for the default.
func (e *Env) resetSystem(ctx context.Context, name string) error {
	if _, err := e.Pool.Exec(ctx, "ALTER SYSTEM RESET "+pgx.Identifier{name}.Sanitize()); err != nil {
		return err
	}
	if _, err := e.Pool.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return err
	}
	return waitFor(ctx, name+" restored", func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_settings
			WHERE name = $1 AND (sourcefile IS NULL
			   OR sourcefile NOT LIKE '%postgresql.auto.conf')`, name)
		return n == 1, err
	})
}

func (e *Env) reloadAndWait(ctx context.Context, name, value string) error {
	if _, err := e.Pool.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return err
	}
	return waitFor(ctx, name+" = "+value, func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_settings
			WHERE name = $1 AND pg_size_bytes(current_setting($1)) = pg_size_bytes($2)`,
			name, value)
		return n == 1, err
	})
}

// sampler counts, every 200 ms while it runs, the samples in which other
// databases' backends waited on LWLocks: load the scenario did not make.
type sampler struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	hot    int
}

// foreignLWLockLimit is the other-database LWLock waiters a sample may
// show; foreignHotSamples such samples contaminate a run.
const (
	foreignLWLockLimit = 4
	foreignHotSamples  = 3
)

func (e *Env) startSampler() *sampler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sampler{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for ctx.Err() == nil {
			n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
				WHERE wait_event_type = 'LWLock' AND state = 'active'
				  AND datname IS DISTINCT FROM current_database()`)
			if err == nil && n >= foreignLWLockLimit {
				s.mu.Lock()
				s.hot++
				s.mu.Unlock()
			}
			_ = sleepRest(ctx, 200*time.Millisecond)
		}
	}()
	return s
}

// stop ends the sampler; a run other databases loaded is contaminated.
func (s *sampler) stop() error {
	if s == nil {
		return nil
	}
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hot >= foreignHotSamples {
		return &Contaminated{Reason: fmt.Sprintf("other databases waited on LWLocks in "+
			"%d samples", s.hot)}
	}
	return nil
}
