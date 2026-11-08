package lakebaseobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func singleConnectionEvidencePool(t *testing.T, s *EvidenceSource) *pgxpool.Pool {
	t.Helper()
	cfg := s.monitored.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestDBHistoricalReadAllowsAcquisitionBeyondOneStatementBudget(t *testing.T) {
	s, scope, now := evidenceFixture(t, SourceRetention)
	seedEvidence(t, s, Plans, now, scopedEvent(t, scope, Plans, now.Add(-time.Minute), now, nil))
	pool := singleConnectionEvidencePool(t, s)
	// Identity and store intentionally share one connection. Identity must
	// release its transaction before history tries to acquire that connection.
	store := *s.store
	store.pool = pool
	source := *s
	source.monitored, source.store = pool, &store
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(held.Release) }
	defer release()
	timer := time.AfterFunc(probes.MaxStatementTimeout+150*time.Millisecond, release)
	defer timer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	r := source.readAt(ctx, probes.LakebasePlans, probes.Args{}, now)
	if r.Status != probes.StatusOK || len(r.Rows) != 1 {
		t.Fatalf("pool acquisition consumed identity query budget: %+v", r)
	}
	if elapsed := time.Since(start); elapsed < probes.MaxStatementTimeout || elapsed >= 3*time.Second {
		t.Fatalf("acquisition did not use bounded whole-read budget: %v", elapsed)
	}
}

func TestDBHistoricalReadAcquisitionHonorsCallerAndWholeReadDeadlines(t *testing.T) {
	s, _, now := evidenceFixture(t, SourceRetention)
	pool := singleConnectionEvidencePool(t, s)
	source := *s
	source.monitored = pool
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	t.Run("earlier caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		r := source.readAt(ctx, probes.LakebasePlans, probes.Args{}, now)
		if r.Status != probes.StatusError || r.Reason != "history_identity_unavailable" || len(r.Rows) != 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("caller deadline was ignored: %+v, %v", r, ctx.Err())
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("extended caller deadline: %v", elapsed)
		}
	})
	t.Run("canceled caller", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		r := source.readAt(ctx, probes.LakebasePlans, probes.Args{}, now)
		if r.Status != probes.StatusError || len(r.Rows) != 0 || time.Since(start) > time.Second {
			t.Fatalf("canceled read waited for fixed budget: %+v", r)
		}
	})
	t.Run("whole read bound", func(t *testing.T) {
		start := time.Now()
		r := source.readAt(context.Background(), probes.LakebasePlans, probes.Args{}, now)
		if r.Status != probes.StatusError || r.Reason != "history_identity_unavailable" || len(r.Rows) != 0 {
			t.Fatalf("unbounded caller escaped acquisition fence: %+v", r)
		}
		if elapsed := time.Since(start); elapsed < evidenceReadTimeout || elapsed > evidenceReadTimeout+time.Second {
			t.Fatalf("whole-read acquisition bound = %v", elapsed)
		}
	})
}

func TestDBHistoricalBackendStatementAndPolicyLockAreBounded(t *testing.T) {
	s, scope, now := evidenceFixture(t, SourceRetention)
	pool := singleConnectionEvidencePool(t, s)
	var beforeStatement, beforeLock string
	if err := pool.QueryRow(context.Background(), `SELECT current_setting('statement_timeout'),current_setting('lock_timeout')`).Scan(&beforeStatement, &beforeLock); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tx, err := beginEvidenceRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackEvidenceRead(tx)
	start := time.Now()
	_, err = tx.Exec(ctx, `SELECT pg_sleep(2)`)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "57014" || ctx.Err() != nil {
		t.Fatalf("backend statement did not stop before whole-read deadline: %v; context=%v", err, ctx.Err())
	}
	if elapsed := time.Since(start); elapsed < probes.MaxStatementTimeout || elapsed > time.Second {
		t.Fatalf("backend statement bound = %v", elapsed)
	}
	rollbackEvidenceRead(tx)
	var afterStatement, afterLock string
	if err := pool.QueryRow(ctx, `SELECT current_setting('statement_timeout'),current_setting('lock_timeout')`).Scan(&afterStatement, &afterLock); err != nil {
		t.Fatal(err)
	}
	if beforeStatement != afterStatement || beforeLock != afterLock {
		t.Fatalf("read settings leaked to next borrower: before %s/%s, after %s/%s", beforeStatement, beforeLock, afterStatement, afterLock)
	}
	// A concurrent policy writer must not make a retained read wait until the
	// longer whole-read deadline or expose events without the policy lock.
	holder, err := s.store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackEvidenceRead(holder)
	if _, err = holder.Exec(ctx, `SELECT 1 FROM sage.lakebaseobs_owner_policy WHERE deployment_id=$1::uuid AND database_id=$2::uuid FOR UPDATE`, scope.DeploymentID, scope.DatabaseID); err != nil {
		t.Fatal(err)
	}
	store := *s.store
	store.pool = pool
	start = time.Now()
	events, _, err := store.evidenceEvents(ctx, scope, s.runtime.binding, Plans, Window{now.Add(-time.Hour), now}, 0, evidenceRows, now)
	if !errors.As(err, &pgerr) || (pgerr.Code != "57014" && pgerr.Code != "55P03") || len(events) != 0 || ctx.Err() != nil {
		t.Fatalf("policy lock did not stop at backend budget: events=%d err=%v context=%v", len(events), err, ctx.Err())
	}
	if elapsed := time.Since(start); elapsed < probes.MaxStatementTimeout || elapsed > time.Second {
		t.Fatalf("policy lock bound = %v", elapsed)
	}
	// A failed read must return its only connection, with no open transaction.
	if err := pool.QueryRow(ctx, `SELECT current_setting('statement_timeout'),current_setting('lock_timeout')`).Scan(&afterStatement, &afterLock); err != nil {
		t.Fatal(err)
	}
	if beforeStatement != afterStatement || beforeLock != afterLock {
		t.Fatal("policy-lock failure leaked local settings")
	}
}

func TestDBHistoricalCanceledReadReleasesPolicyLockAndConnection(t *testing.T) {
	s, scope, _ := evidenceFixture(t, SourceRetention)
	pool := singleConnectionEvidencePool(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx, err := beginEvidenceRead(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackEvidenceRead(tx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sage.lakebaseobs_owner_policy WHERE deployment_id=$1::uuid AND database_id=$2::uuid FOR SHARE`, scope.DeploymentID, scope.DatabaseID); err != nil {
		t.Fatal(err)
	}
	cancel()
	rollbackEvidenceRead(tx)
	// The canceled caller must leave neither a checked-out sole connection
	// nor a policy lock that blocks the next owner-policy writer.
	check, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := pool.Exec(check, `UPDATE sage.lakebaseobs_owner_policy SET allow_payload=allow_payload WHERE deployment_id=$1::uuid AND database_id=$2::uuid`, scope.DeploymentID, scope.DatabaseID); err != nil {
		t.Fatalf("canceled read retained its connection or policy lock: %v", err)
	}
}

type evidenceRollbackRecorder struct {
	pgx.Tx
	ctx context.Context
}

func (r *evidenceRollbackRecorder) Rollback(ctx context.Context) error {
	r.ctx = ctx
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func TestHistoricalRollbackUsesIndependentBoundedCleanup(t *testing.T) {
	r := &evidenceRollbackRecorder{}
	start := time.Now()
	rollbackEvidenceRead(r)
	if r.ctx == nil {
		t.Fatal("rollback was not attempted")
	}
	deadline, ok := r.ctx.Deadline()
	if !ok || deadline.Before(start.Add(evidenceRollbackTimeout)) || deadline.After(time.Now().Add(evidenceRollbackTimeout)) {
		t.Fatalf("rollback did not receive its independent fixed deadline: %v, %v", deadline, ok)
	}
	if !errors.Is(r.ctx.Err(), context.Canceled) {
		t.Fatal("cleanup context was not canceled after rollback returned")
	}
}
