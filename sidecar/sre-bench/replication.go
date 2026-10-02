package srebench

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// A logical replication consumer on a test_decoding slot. Logical
// walsenders authenticate like ordinary connections to the database (a
// default pg_hba.conf has no "replication" entry for remote hosts), so
// the fixture runs wherever the bench can connect. A consumer that never
// confirms is a slow consumer; one that confirms what it receives keeps
// its slot's retention small.

// statusInterval spaces an acknowledging consumer's standby status
// updates and bounds each receive.
const statusInterval = 200 * time.Millisecond

// pgEpoch is the PostgreSQL protocol's clock origin.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// walMessage is the end position a CopyData message of the stream
// reports, and whether the server asks for an immediate reply.
type walMessage struct {
	end   uint64
	reply bool
}

// parseWALMessage decodes XLogData ('w') and primary keepalive ('k')
// messages; anything else, or a truncated message, is not one.
func parseWALMessage(data []byte) (walMessage, bool) {
	switch {
	case len(data) >= 25 && data[0] == 'w':
		start := binary.BigEndian.Uint64(data[1:9])
		return walMessage{end: start + uint64(len(data)-25)}, true
	case len(data) >= 18 && data[0] == 'k':
		return walMessage{end: binary.BigEndian.Uint64(data[1:9]), reply: data[17] != 0},
			true
	}
	return walMessage{}, false
}

// standbyStatus is a standby status update ('r') confirming pos as
// written, flushed and applied.
func standbyStatus(pos uint64, now time.Time) []byte {
	b := make([]byte, 34)
	b[0] = 'r'
	binary.BigEndian.PutUint64(b[1:9], pos)
	binary.BigEndian.PutUint64(b[9:17], pos)
	binary.BigEndian.PutUint64(b[17:25], pos)
	binary.BigEndian.PutUint64(b[25:33], uint64(now.Sub(pgEpoch).Microseconds()))
	return b
}

// createLogicalSlot creates a test_decoding slot. Creation waits for the
// cluster's running transactions to finish; one that outlasts the bound
// is another package's long transaction, not the scenario.
func createLogicalSlot(ctx context.Context, e *Env, slot string) error {
	var level string
	if err := e.Pool.QueryRow(ctx, "SELECT current_setting('wal_level')").
		Scan(&level); err != nil {
		return err
	}
	if level != "logical" {
		return &Unsupported{Reason: "wal_level is " + level + ", not logical"}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := e.Pool.Exec(cctx,
		"SELECT pg_create_logical_replication_slot($1, 'test_decoding')", slot)
	if err != nil && cctx.Err() != nil && ctx.Err() == nil {
		return &Contaminated{Reason: "logical slot creation waited 30 s on other " +
			"transactions"}
	}
	return err
}

// consumer is a running logical replication stream.
type consumer struct {
	conn   *pgconn.PgConn
	cancel context.CancelFunc
	done   chan struct{}
	err    error // why the stream ended; read after done
}

// alive reports a stream that ended before it was stopped.
func (c *consumer) alive() error {
	select {
	case <-c.done:
		return fmt.Errorf("replication stream ended: %w", c.err)
	default:
		return nil
	}
}

// stop ends the stream and closes its connection.
func (c *consumer) stop() {
	c.cancel()
	<-c.done
	_ = c.conn.Close(context.Background())
}

// startLogicalConsumer streams slot; with ack it confirms what it
// receives.
func startLogicalConsumer(ctx context.Context, e *Env, slot string,
	ack bool) (*consumer, error) {
	cfg, err := pgconn.ParseConfig(e.DSN)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["replication"] = "database"
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
		"START_REPLICATION SLOT %s LOGICAL 0/0", pgx.Identifier{slot}.Sanitize())})
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
	c := &consumer{conn: conn, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		c.err = consume(streamCtx, conn, ack)
	}()
	return c, nil
}

// consume drains the stream until ctx ends (nil) or the stream breaks
// (its error); with ack it confirms the furthest position seen every
// statusInterval and whenever the server asks.
func consume(ctx context.Context, conn *pgconn.PgConn, ack bool) error {
	var pos uint64
	next := time.Now()
	for ctx.Err() == nil {
		if ack && !time.Now().Before(next) {
			if err := sendStatus(conn, pos); err != nil {
				return err
			}
			next = time.Now().Add(statusInterval)
		}
		rctx, cancel := context.WithTimeout(ctx, statusInterval)
		msg, err := conn.ReceiveMessage(rctx)
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil && pgconn.Timeout(err):
			continue
		case err != nil:
			return err
		}
		cd, ok := msg.(*pgproto3.CopyData)
		if !ok {
			continue
		}
		m, ok := parseWALMessage(cd.Data)
		if ok && m.end > pos {
			pos = m.end
		}
		if ok && m.reply && ack {
			next = time.Now()
		}
	}
	return nil
}

func sendStatus(conn *pgconn.PgConn, pos uint64) error {
	conn.Frontend().Send(&pgproto3.CopyData{Data: standbyStatus(pos, time.Now())})
	return conn.Frontend().Flush()
}
