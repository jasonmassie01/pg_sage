package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// xminHoldersSQL is the shared xmin_horizon query (sessions by snapshot
// or XID, standbys' feedback, prepared transactions, slots' xmin and
// catalog_xmin), oldest first; tests replace it to fail the read.
var xminHoldersSQL = probes.XminHorizonSQL

// xminHolderLimit bounds the holders read per scan.
const xminHolderLimit = 20

// sessionDetailReader fills a pinning session's own state; false means
// the session (that PID with that backend start) is gone.
type sessionDetailReader func(context.Context, *freeze.XminBlocker) (bool, error)

// protectedApplications are never signalled: dumps, base backups,
// restores and pg_sage itself. They are still holders, and are named.
var protectedApplications = []string{"pg_dump", "pg_basebackup", "pg_restore", "pg_sage"}

func protectedApplication(app string) bool {
	app = strings.ToLower(app)
	for _, name := range protectedApplications {
		if strings.Contains(app, name) {
			return true
		}
	}
	return false
}

// readXminHolders reads every holder of this database's xmin horizon
// through q, once per sampler reading. A failed read aborts the reading.
func readXminHolders(
	ctx context.Context, q catalogread.Querier,
) ([]freeze.XminBlocker, error) {
	rows, err := q.Query(ctx, xminHoldersSQL, xminHolderLimit)
	if err != nil {
		return nil, fmt.Errorf("read xmin holders: %w", err)
	}
	defer rows.Close()
	var holders []freeze.XminBlocker
	for rows.Next() {
		holder, err := scanXminHolder(rows)
		if err != nil {
			return nil, err
		}
		holders = append(holders, holder)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read xmin holders: %w", err)
	}
	return holders, nil
}

// scanXminHolder scans one xmin_horizon row. An unknown age reads as
// zero: such a holder pins nothing, and is never taken to be absent.
func scanXminHolder(row pgx.Row) (freeze.XminBlocker, error) {
	var h freeze.XminBlocker
	var pid, age *int64
	var start *time.Time
	var name, app *string
	var xactAge *float64
	var active *bool
	if err := row.Scan(&h.Kind, &pid, &start, &name, &h.State, &age, &xactAge, &app,
		&active); err != nil {
		return freeze.XminBlocker{}, fmt.Errorf("scan xmin holder: %w", err)
	}
	if pid != nil {
		h.PID = int(*pid)
	}
	if start != nil {
		h.BackendStart = *start
	}
	if name != nil {
		h.Name = *name
	}
	if app != nil {
		h.AppName = *app
	}
	if age != nil {
		h.XminAge = *age
	}
	h.Protected = h.Kind == freeze.HolderSession && protectedApplication(h.AppName)
	return h, nil
}

// readSessionDetail reads the session's user, query and its own XID and
// snapshot ages, by PID and backend start (a reused PID is another
// session).
func (c *PostgresFreezeCustodian) readSessionDetail(
	ctx context.Context, h *freeze.XminBlocker,
) (bool, error) {
	err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(usename, ''),
		LEFT(COALESCE(query, ''), 500), COALESCE(query_start, backend_start),
		age(backend_xid)::bigint, age(backend_xmin)::bigint
		FROM pg_stat_activity WHERE pid = $1 AND backend_start = $2`,
		h.PID, h.BackendStart).Scan(&h.User, &h.Query, &h.QueryStart,
		&h.BackendXIDAge, &h.BackendXminAge)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// holderPin is one holder and the red tables it pins in a scan.
type holderPin struct {
	holder   freeze.XminBlocker
	targets  []string
	deadline *policy.DeadlineContext
}

// holderPins collects the pinned tables per holder, in scan order.
type holderPins struct {
	order []string
	byKey map[string]*holderPin
}

func (p *holderPins) add(h freeze.XminBlocker, target string, deadline *policy.DeadlineContext) {
	if p.byKey == nil {
		p.byKey = map[string]*holderPin{}
	}
	key := freeze.HolderKey(h)
	pin, ok := p.byKey[key]
	if !ok {
		pin = &holderPin{holder: h}
		p.byKey[key] = pin
		p.order = append(p.order, key)
	}
	pin.targets = append(pin.targets, target)
	if deadline != nil && (pin.deadline == nil || deadline.HardAt.Before(pin.deadline.HardAt)) {
		pin.deadline = deadline
	}
}

// plan answers every table, then makes one proposal per pinning holder
// (the root cause first) listing every table it pins, and one plan for
// the red system catalogs (the custodian never vacuums a catalog).
func (c *PostgresFreezeCustodian) plan(
	ctx context.Context, horizons []freezeRow, rates freezeRateSample,
	holders []freeze.XminBlocker, repack bool,
) ([]Proposal, error) {
	var pins holderPins
	var catalogs catalogPlan
	tables := make([]Proposal, 0, len(horizons))
	for _, row := range horizons {
		if row.catalog {
			if err := catalogs.add(c, row, rates); err != nil {
				return nil, err
			}
			continue
		}
		proposal, pinnedBy, err := c.respond(ctx, row, rates, holders, repack)
		if err != nil {
			return nil, err
		}
		if pinnedBy != nil {
			pins.add(*pinnedBy, row.sample.Schema+"."+row.sample.Table, proposal.Deadline)
		}
		if proposal.SQL != "" || proposal.Plan != "" {
			tables = append(tables, proposal)
		}
	}
	result, err := c.holderProposals(ctx, pins)
	if err != nil {
		return nil, err
	}
	proposal, ok, err := catalogs.proposal(c.database)
	if err != nil {
		return nil, err
	}
	if ok {
		result = append(result, proposal)
	}
	return append(result, tables...), nil
}

func (c *PostgresFreezeCustodian) holderProposals(
	ctx context.Context, pins holderPins,
) ([]Proposal, error) {
	result := make([]Proposal, 0, len(pins.order))
	for _, key := range pins.order {
		pin := pins.byKey[key]
		h := pin.holder
		if h.Kind == freeze.HolderSession && !h.Protected {
			found, err := c.detail(ctx, &h)
			if err != nil {
				return nil, fmt.Errorf("read xmin holder session pid %d: %w", h.PID, err)
			}
			if !found {
				continue // it ended since the holder read: nothing to signal
			}
		}
		response, err := freeze.HolderResponse(h, pin.targets)
		if err != nil {
			return nil, fmt.Errorf("answer xmin holder: %w", err)
		}
		result = append(result, Proposal{Database: c.database, Feature: "freeze_blocker",
			SQL: response.SQL, Plan: response.Plan, ResponseKind: string(response.Kind),
			Evidence: response.Evidence, TargetObjects: pin.targets, Deadline: pin.deadline})
	}
	return result, nil
}
