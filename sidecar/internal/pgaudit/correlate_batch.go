package pgaudit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// An untagged pg_sage record (a binary from before action tags, or an
// action with no decision) waits for its action row, written only after
// the statement returns: at most holdFor from when it was first seen, and
// at most maxHeld records at once (the oldest go first).
const (
	holdFor = 5 * time.Minute
	maxHeld = 1000
)

// heldRecord is one of pg_sage's untagged records waiting for its action.
type heldRecord struct {
	entry logwatch.LogEntry
	rec   Record
	since time.Time // when the correlator first saw it
}

// batch is one ingest's work: the rows to store, the records still held,
// and each decision's action, looked up once per batch.
type batch struct {
	c         *Correlator
	own       string
	roles     RoleMap // the agent roles' principals; nil when unread
	now       time.Time
	st        Stats
	rows      []event
	held      []heldRecord
	decisions map[int64]int64 // decision id -> its action id (0: none yet)
}

// retryHeld looks the held records up again; one held longer than holdFor
// is dropped as unmatched without another lookup.
func (b *batch) retryHeld(ctx context.Context, held []heldRecord) error {
	for _, h := range held {
		if b.now.Sub(h.since) > holdFor {
			b.st.Unmatched++
			continue
		}
		if err := b.addOwn(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

// add correlates one log entry.
func (b *batch) add(ctx context.Context, e logwatch.LogEntry) error {
	rec, ok := Parse(e.Message)
	if !ok {
		b.st.NotAudit++
		return nil
	}
	a, ok := Attribute(e, b.own, b.roles)
	switch {
	case !ok:
		b.st.Dropped++
	case a.By == ByActionTag:
		id, err := b.actionOfDecision(ctx, a.DecisionID)
		if err != nil {
			return err
		}
		a.ActionID = id
		b.rows = append(b.rows, b.c.row(e, rec, a))
	case a.By == ByPGSage:
		if rec.Class == "READ" { // pg_sage's monitoring, never an action
			b.st.Dropped++
			return nil
		}
		return b.addOwn(ctx, heldRecord{entry: e, rec: rec, since: b.now})
	default:
		b.rows = append(b.rows, b.c.row(e, rec, a))
	}
	return nil
}

// addOwn links an untagged pg_sage record to its action by its SQL. An
// unmatched record that may be an action still running is held; any other
// is dropped.
func (b *batch) addOwn(ctx context.Context, h heldRecord) error {
	id, err := b.c.actionFor(ctx, h.rec.Statement, h.entry.Timestamp)
	if err != nil {
		return err
	}
	switch {
	case id > 0:
		b.rows = append(b.rows, b.c.row(h.entry, h.rec,
			Attribution{By: ByStatement, ActionID: id}))
	case holdable(h.rec):
		b.held = append(b.held, h)
	default:
		b.st.Dropped++
	}
	return nil
}

// holdable reports whether an unmatched record may be an action whose row
// is not written yet: DDL (index builds) or MISC (VACUUM), but not the
// SET and RESET of pg_sage's session setup, and not pg_sage's upkeep of
// its own sage schema (bootstrap DDL): no action row ever has those, and
// holding them would push real actions' records out of the queue.
func holdable(rec Record) bool {
	if strings.HasPrefix(rec.ObjectName, "sage.") {
		return false
	}
	switch rec.Class {
	case "DDL":
		return true
	case "MISC":
		return rec.Command != "SET" && rec.Command != "RESET"
	}
	return false
}

// capHeld keeps the newest maxHeld held records.
func (b *batch) capHeld() {
	if over := len(b.held) - maxHeld; over > 0 {
		b.st.Overflow += over
		b.held = b.held[over:]
	}
}

// actionOfDecision is the first action row of decision (0 when it has
// none yet), through idx_action_log_decision.
func (b *batch) actionOfDecision(ctx context.Context, decision int64) (int64, error) {
	if id, ok := b.decisions[decision]; ok {
		return id, nil
	}
	var id int64
	err := b.c.q.QueryRow(ctx, `/* pg_sage pgaudit v1 */ SELECT id FROM sage.action_log
		WHERE decision_id = $1 ORDER BY id LIMIT 1`, decision).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("pgaudit: find the action of decision %d: %w", decision, err)
	}
	b.decisions[decision] = id
	return id, nil
}
