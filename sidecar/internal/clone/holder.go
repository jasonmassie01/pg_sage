package clone

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Holder liveness (lab-05a): a receipt names the process holding its
// clone and when it last heartbeated. A reaper of another process reaps
// a clone whose holder stopped heartbeating before its TTL, so a clone a
// killed sidecar left (a full copy's TTL is hours) does not outlive it.

const (
	// DefaultHeartbeatEvery is how often a holder heartbeats its clones.
	DefaultHeartbeatEvery = time.Minute
	// DefaultHolderStaleAfter is how old a heartbeat is before its holder
	// is taken for dead: five missed heartbeats.
	DefaultHolderStaleAfter = 5 * time.Minute
)

var processHolder = sync.OnceValue(func() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; the pid and
		// clock still make the id unique to this process.
		b = []byte(time.Now().Format("150405.000000"))
	}
	host, _ := os.Hostname()
	if len(host) > 100 {
		host = host[:100]
	}
	return fmt.Sprintf("%s/%d/%x", host, os.Getpid(), b)
})

// ProcessHolder is this process's holder id: host, pid and a random part.
func ProcessHolder() string { return processHolder() }

const recordReceiptSQL = `/* pg_sage clone_receipt v2 */
INSERT INTO sage.clone_instances (deployment_id, adapter, scope, name, purpose,
    principal_id, source_database_id, masked, status, expires_at, holder_id,
    heartbeat_at, estimate)
SELECT $1::uuid, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::uuid, $8, 'creating', $9,
    NULLIF($10::text, ''), CASE WHEN $10::text <> '' THEN now() END,
    NULLIF($11::text, '')::jsonb
WHERE $12::int <= 0 OR (SELECT count(*) FROM sage.clone_instances
    WHERE deployment_id = $1::uuid AND created_at > now() - interval '1 hour') < $12::int
RETURNING ` + receiptColumns

// createLockSQL serializes the creates of one deployment, so the hourly
// count and the INSERT it allows are one decision.
const createLockSQL = `/* pg_sage clone_receipt v2 */
SELECT pg_advisory_xact_lock(hashtextextended('sage.clone_creates:' || $1, 0))`

// RecordLimited is Record when the deployment made fewer than perHour
// receipts in the last hour (all purposes), else ErrCreateRateLimited;
// perHour <= 0 is no limit.
func (s *ReceiptStore) RecordLimited(ctx context.Context, r Receipt, perHour int) (Receipt,
	error) {
	if err := validateReceipt(r, time.Now()); err != nil {
		return Receipt{}, err
	}
	if s == nil || s.pool == nil {
		return Receipt{}, ErrNoReceiptStore
	}
	estimate := ""
	if r.Estimate != nil {
		b, err := json.Marshal(r.Estimate)
		if err != nil {
			return Receipt{}, fmt.Errorf("encode clone receipt %s estimate: %w", r.Name, err)
		}
		estimate = string(b)
	}
	args := []any{r.DeploymentID, r.Adapter, r.Scope, r.Name, r.Purpose, r.PrincipalID,
		r.SourceDatabaseID, r.Masked, r.ExpiresAt, r.HolderID, estimate, perHour}
	if perHour <= 0 {
		got, err := scanReceipt(s.pool.QueryRow(ctx, recordReceiptSQL, args...))
		return recorded(r, got, err, perHour)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, fmt.Errorf("record clone receipt %s: begin: %w", r.Name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, createLockSQL, r.DeploymentID); err != nil {
		return Receipt{}, fmt.Errorf("record clone receipt %s: lock creates: %w", r.Name, err)
	}
	got, err := scanReceipt(tx.QueryRow(ctx, recordReceiptSQL, args...))
	if got, err = recorded(r, got, err, perHour); err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Receipt{}, fmt.Errorf("record clone receipt %s: commit: %w", r.Name, err)
	}
	return got, nil
}

// recorded maps an INSERT's outcome to the receipt errors.
func recorded(r Receipt, got Receipt, err error, perHour int) (Receipt, error) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Receipt{}, fmt.Errorf("%w: clone.max_creates_per_hour (%d) clones were "+
			"created for this deployment in the last hour; nothing was created, retry "+
			"once the oldest is an hour old", ErrCreateRateLimited, perHour)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return Receipt{}, fmt.Errorf("%w: %s/%s/%s", ErrReceiptExists, r.Adapter, r.Scope,
			r.Name)
	case err != nil:
		return Receipt{}, fmt.Errorf("record clone receipt %s: %w", r.Name, err)
	}
	return got, nil
}

// hold starts heartbeating receipt id, starting the heartbeat loop when
// it is not running and the process is alive.
func (p *ReceiptingProvider) hold(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held[id] = true
	if !p.beating && p.cfg.Lifetime.Err() == nil {
		p.beating = true
		go p.heartbeatLoop()
	}
}

// release stops heartbeating receipt id: its create failed (no one holds
// an uncertain create) or its clone is being destroyed.
func (p *ReceiptingProvider) release(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, id)
}

// heldIDs is the receipts to heartbeat; with none it ends the loop.
func (p *ReceiptingProvider) heldIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.held) == 0 || p.cfg.Lifetime.Err() != nil {
		p.beating = false
		return nil
	}
	ids := make([]int64, 0, len(p.held))
	for id := range p.held {
		ids = append(ids, id)
	}
	return ids
}

func (p *ReceiptingProvider) heartbeatLoop() {
	ticker := time.NewTicker(p.cfg.HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.cfg.Lifetime.Done():
			p.heldIDs()
			return
		case <-ticker.C:
		}
		ids := p.heldIDs()
		if ids == nil {
			return
		}
		ctx, cancel := context.WithTimeout(p.cfg.Lifetime, cleanupTimeout)
		_, err := p.store.Heartbeat(ctx, p.cfg.Holder, ids)
		cancel()
		p.mu.Lock()
		p.beatErr = err
		p.mu.Unlock()
	}
}

// HeartbeatErr is the last heartbeat's error, nil once one succeeds. A
// holder that cannot heartbeat for DefaultHolderStaleAfter may have its
// clones reaped by another process.
func (p *ReceiptingProvider) HeartbeatErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.beatErr
}
