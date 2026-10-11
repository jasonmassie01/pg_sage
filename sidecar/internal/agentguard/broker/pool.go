package broker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// loginPool is one principal's broker-login pool on one database. The
// credential's fingerprint detects a rotation: the next call replaces it.
type loginPool struct {
	pool        *pgxpool.Pool
	principalID string
	credential  [32]byte
}

// pools keeps the broker's login pools and its total connection budget.
type pools struct {
	mu     sync.Mutex
	byKey  map[string]*loginPool
	budget chan struct{}
	cfg    Config
}

func newPools(cfg Config) *pools {
	return &pools{byKey: map[string]*loginPool{}, budget: make(chan struct{},
		cfg.MaxTotalConns), cfg: cfg}
}

// acquire takes one connection of the total budget, then one connection of
// the principal's pool, which logs in as role with password. release
// returns both.
func (p *pools) acquire(ctx context.Context, t Target, principalID, principalName, role,
	password string) (*pgxpool.Conn, func(), error) {
	select {
	case p.budget <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	lp, err := p.poolFor(ctx, t, principalID, principalName, role, password)
	if err != nil {
		<-p.budget
		return nil, nil, err
	}
	conn, err := lp.Acquire(ctx)
	if err != nil {
		<-p.budget
		return nil, nil, err
	}
	return conn, func() { conn.Release(); <-p.budget }, nil
}

func (p *pools) poolFor(ctx context.Context, t Target, principalID, principalName, role,
	password string) (*pgxpool.Pool, error) {
	key := principalID + "\x00" + t.Name
	sum := sha256.Sum256([]byte(role + "\x00" + password))
	p.mu.Lock()
	defer p.mu.Unlock()
	if lp, ok := p.byKey[key]; ok {
		if lp.credential == sum {
			return lp.pool, nil
		}
		lp.pool.Close()
		delete(p.byKey, key)
	}
	cfg, err := p.poolConfig(t, principalName, role, password)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open the broker pool of %s on %s: %w", principalID, t.Name, err)
	}
	p.byKey[key] = &loginPool{pool: pool, principalID: principalID, credential: sum}
	return pool, nil
}

// poolConfig copies pg_sage's connection target (host, port, database,
// TLS) and replaces the login with the broker role's.
func (p *pools) poolConfig(t Target, principalName, role, password string) (
	*pgxpool.Config, error) {
	if t.Pool == nil {
		return nil, fmt.Errorf("%w: no connection target for %s", ErrUnavailable, t.Name)
	}
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("broker pool config: %w", err)
	}
	conn := t.Pool.Config().ConnConfig.Copy()
	conn.User, conn.Password = role, password
	if conn.RuntimeParams == nil {
		conn.RuntimeParams = map[string]string{}
	}
	conn.RuntimeParams["application_name"] = applicationName(principalName)
	// PgBouncer transaction pooling: no server-side prepared statements.
	conn.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.ConnConfig = conn
	cfg.MaxConns = int32(p.cfg.PoolMaxConns)
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = p.cfg.PoolIdle
	return cfg, nil
}

// applicationName tags brokered sessions for pg_stat_activity and pgaudit.
func applicationName(principalName string) string {
	name := "pg_sage agent:" + principalName
	return strings.ToValidUTF8(name, "")
}

// evict closes every pool of a principal ("" closes all).
func (p *pools) evict(principalID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, lp := range p.byKey {
		if principalID == "" || lp.principalID == principalID {
			lp.pool.Close()
			delete(p.byKey, key)
		}
	}
}

// drop closes one pool whose login failed, so the next call reconnects.
func (p *pools) drop(principalID, database string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := principalID + "\x00" + database
	if lp, ok := p.byKey[key]; ok {
		lp.pool.Close()
		delete(p.byKey, key)
	}
}
