package collector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// indexDefScratchMin is the number of index definitions one page must
// fetch before the fetch moves to a scratch connection. pg_get_indexdef
// fills the backend's catalog cache (CacheMemoryContext) by ~4 KB per
// index and a backend never gives that memory back: on lifeos one full
// pass would leave a pool backend holding ~150 MB. Bulk fetches (the first
// cycle, a column rename, the hourly backstop) therefore run on a
// connection taken out of the pool and closed when the pass ends.
const indexDefScratchMin = 100

// scratchConn is a connection hijacked from the pool for one pass.
type scratchConn struct {
	pool *pgxpool.Pool
	conn *pgx.Conn
}

func (s *scratchConn) get(ctx context.Context) (*pgx.Conn, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire scratch catalog connection: %w", err)
	}
	s.conn = pc.Hijack()
	return s.conn, nil
}

// close ends the scratch backend, releasing its catalog cache.
func (s *scratchConn) close() error {
	if s.conn == nil {
		return nil
	}
	conn := s.conn
	s.conn = nil
	if err := conn.Close(context.Background()); err != nil {
		return fmt.Errorf("close scratch catalog connection: %w", err)
	}
	return nil
}
