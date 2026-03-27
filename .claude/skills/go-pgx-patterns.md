# Go Sidecar + pgx Patterns for pg_sage

## pgxpool Setup

```go
package db

import (
    "context"
    "fmt"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
)

func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
    config, err := pgxpool.ParseConfig(dsn)
    if err != nil {
        return nil, fmt.Errorf("parse dsn: %w", err)
    }

    // Sidecar-appropriate pool settings
    config.MaxConns = 5              // pg_sage doesn't need many — it's a monitoring tool
    config.MinConns = 2              // Keep 2 warm for collector + analyzer
    config.MaxConnLifetime = 30 * time.Minute
    config.MaxConnIdleTime = 5 * time.Minute
    config.HealthCheckPeriod = 30 * time.Second

    pool, err := pgxpool.NewWithConfig(ctx, config)
    if err != nil {
        return nil, fmt.Errorf("create pool: %w", err)
    }

    if err := pool.Ping(ctx); err != nil {
        pool.Close()
        return nil, fmt.Errorf("ping: %w", err)
    }

    return pool, nil
}
```

## Querying System Catalogs

Always use parameterized queries. Always handle context cancellation.

```go
func (c *Collector) queryStatStatements(ctx context.Context) ([]models.StatementStat, error) {
    rows, err := c.pool.Query(ctx, `
        SELECT queryid, query, calls, total_exec_time, mean_exec_time,
               rows, shared_blks_hit, shared_blks_read
        FROM pg_stat_statements
        WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
        ORDER BY total_exec_time DESC
        LIMIT $1
    `, c.cfg.TopQueriesLimit)
    if err != nil {
        return nil, fmt.Errorf("query pg_stat_statements: %w", err)
    }
    defer rows.Close()

    var stats []models.StatementStat
    for rows.Next() {
        var s models.StatementStat
        if err := rows.Scan(&s.QueryID, &s.Query, &s.Calls, &s.TotalExecTime,
            &s.MeanExecTime, &s.Rows, &s.SharedBlksHit, &s.SharedBlksRead); err != nil {
            return nil, fmt.Errorf("scan statement stat: %w", err)
        }
        stats = append(stats, s)
    }
    return stats, rows.Err()
}
```

## PG Version Detection

pg_sage targets PG 14+. Some system views changed between versions.

```go
func (c *Collector) pgVersion(ctx context.Context) (int, error) {
    var version int
    err := c.pool.QueryRow(ctx, "SHOW server_version_num").Scan(&version)
    if err != nil {
        return 0, fmt.Errorf("get pg version: %w", err)
    }
    return version, nil // e.g. 170001 for PG 17.1
}
```

## LISTEN/NOTIFY (pgx native)

Useful for real-time event handling (e.g., config changes, emergency stop):

```go
func (s *Sidecar) listenForCommands(ctx context.Context) error {
    conn, err := s.pool.Acquire(ctx)
    if err != nil {
        return fmt.Errorf("acquire conn for listen: %w", err)
    }
    defer conn.Release()

    _, err = conn.Exec(ctx, "LISTEN sage_control")
    if err != nil {
        return fmt.Errorf("listen: %w", err)
    }

    for {
        notification, err := conn.Conn().WaitForNotification(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return nil // clean shutdown
            }
            return fmt.Errorf("wait notification: %w", err)
        }

        switch notification.Payload {
        case "emergency_stop":
            s.emergencyStop()
        case "resume":
            s.resume()
        case "reload_config":
            s.reloadConfig(ctx)
        }
    }
}
```

## Goroutine Lifecycle (errgroup)

```go
func run(ctx context.Context) error {
    ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    pool, err := db.NewPool(ctx, cfg.DSN)
    if err != nil {
        return err
    }
    defer pool.Close()

    g, ctx := errgroup.WithContext(ctx)

    g.Go(func() error { return collector.Run(ctx, pool, cfg) })
    g.Go(func() error { return analyzer.Run(ctx, pool, cfg) })
    g.Go(func() error { return briefing.Run(ctx, pool, cfg) })

    return g.Wait()
}
```

## Error Handling Patterns

```go
// Sentinel errors for known conditions
var (
    ErrCircuitOpen    = errors.New("circuit breaker is open")
    ErrTrustTooLow    = errors.New("trust level insufficient for action")
    ErrLLMUnavailable = errors.New("LLM provider unavailable")
)

// Wrap with context at every layer
func (a *Analyzer) analyzeCycle(ctx context.Context) error {
    snapshot, err := a.collector.LatestSnapshot(ctx)
    if err != nil {
        return fmt.Errorf("get latest snapshot: %w", err)
    }
    // ...
}

// Check sentinel errors with errors.Is
if errors.Is(err, ErrCircuitOpen) {
    slog.Warn("skipping action, circuit breaker open", "action", action.Name)
    return nil // don't propagate — this is expected
}
```

## Interface Design

Define at the consumer. Keep small.

```go
// In internal/analyzer — what it needs from the collector
type SnapshotSource interface {
    LatestSnapshot(ctx context.Context) (*models.Snapshot, error)
}

// In internal/executor — what it needs from the analyzer
type FindingSource interface {
    ActionableFindings(ctx context.Context) ([]models.Finding, error)
}

// In internal/briefing — what it needs from the LLM
type LLMProvider interface {
    Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}
```

## Common Mistakes to Avoid

1. Using `database/sql` instead of pgx native — loses pgx's performance and features
2. Forgetting to `defer rows.Close()` — connection leak
3. Ignoring `rows.Err()` after iteration — missed errors
4. String-concatenating SQL — SQL injection, use `$1` params
5. Not passing `context.Context` — can't cancel, can't timeout
6. Spawning goroutines without lifecycle management — goroutine leak
7. Using `sync.Mutex` when a channel would be cleaner
8. `log.Fatal` in library code — only acceptable in `main()`
9. Swallowing errors with `_ = someFunc()` — at minimum, log them
10. Creating a new pool per query — use `pgxpool`, acquire/release connections
