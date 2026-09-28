package srebench

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// startPhysicalConsumer streams a physical slot and never confirms any
// WAL: the slot is active, but its restart position does not move, like
// a consumer that cannot keep up. stop ends the stream.
func startPhysicalConsumer(ctx context.Context, e *Env, slot string) (func(), error) {
	var lsn string
	if err := e.Pool.QueryRow(ctx, `SELECT restart_lsn::text
		FROM pg_catalog.pg_replication_slots WHERE slot_name = $1`, slot).
		Scan(&lsn); err != nil {
		return nil, err
	}
	cfg, err := pgconn.ParseConfig(e.DSN)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["replication"] = "true"
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return nil, &Unsupported{Reason: "replication connections refused: " +
				pgErr.Message}
		}
		return nil, err
	}
	conn.Frontend().Send(&pgproto3.Query{String: fmt.Sprintf(
		"START_REPLICATION SLOT %s PHYSICAL %s", slot, lsn)})
	if err := conn.Frontend().Flush(); err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	msg, err := conn.ReceiveMessage(ctx)
	if _, ok := msg.(*pgproto3.CopyBothResponse); err != nil || !ok {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("start replication: %T %v", msg, err)
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for streamCtx.Err() == nil {
			if _, err := conn.ReceiveMessage(streamCtx); err != nil {
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
		_ = conn.Close(context.Background())
	}, nil
}
