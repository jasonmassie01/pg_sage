package decommission

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Acknowledgement sources (§12 step 4).
const (
	SourceAPI  = "api"
	SourceYAML = "yaml"
)

var (
	// ErrNotExported refuses an acknowledgement that does not confirm the
	// inventory was exported first.
	ErrNotExported = errors.New("exported must be true: export the inventory " +
		"(GET " + InventoryPath + ") before acknowledging it")
	// ErrNoResources refuses an acknowledgement that names no resource.
	ErrNoResources = errors.New("acknowledged_resources must name at least one " +
		"inventory item id")
	// ErrAckTableMissing means the schema bootstrap has not created
	// sage.agentdb_decommission on the control database.
	ErrAckTableMissing = errors.New("sage.agentdb_decommission does not exist: " +
		"start pg_sage once so its schema bootstrap creates it")
)

// UnknownResourcesError lists acknowledged ids that are not in the current
// inventory; nothing is recorded when it is returned.
type UnknownResourcesError struct{ IDs []string }

func (e *UnknownResourcesError) Error() string {
	return "not in the current inventory: " + strings.Join(e.IDs, ", ")
}

// AckRequest is the body of POST AckPath and the YAML agentdb_decommission
// section.
type AckRequest struct {
	AcknowledgedResources []string `json:"acknowledged_resources" yaml:"acknowledged_resources"`
	Exported              bool     `json:"exported" yaml:"exported"`
}

// AckResult splits the ids into newly and previously acknowledged.
type AckResult struct {
	Acknowledged        []string `json:"acknowledged"`
	AlreadyAcknowledged []string `json:"already_acknowledged"`
}

// Acknowledge records that the operator exported the inventory and dealt with
// the named items. It is all or nothing: an unknown id records none. A
// resource keeps its first acknowledgement and actor.
func Acknowledge(ctx context.Context, pool *pgxpool.Pool, req AckRequest,
	actor, source string) (AckResult, error) {
	ids, err := validateAck(req, actor, source)
	if err != nil {
		return AckResult{}, err
	}
	inv, err := Build(ctx, pool)
	if err != nil {
		return AckResult{}, err
	}
	if missing := unknownIDs(inv, ids); len(missing) > 0 {
		return AckResult{}, &UnknownResourcesError{IDs: missing}
	}
	if err := ensureAckTable(ctx, pool); err != nil {
		return AckResult{}, err
	}
	rows, err := pool.Query(ctx, `/* pg_sage agentdb_decommission v1 */
		INSERT INTO sage.agentdb_decommission
			(resource_id, exported, acknowledged_by, source)
		SELECT unnest($1::text[]), true, $2, $3
		ON CONFLICT (resource_id) DO NOTHING
		RETURNING resource_id`, ids, actor, source)
	if err != nil {
		return AckResult{}, fmt.Errorf("record acknowledgement: %w", err)
	}
	inserted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return AckResult{}, fmt.Errorf("record acknowledgement: %w", err)
	}
	return splitResult(ids, inserted), nil
}

func validateAck(req AckRequest, actor, source string) ([]string, error) {
	if source != SourceAPI && source != SourceYAML {
		return nil, fmt.Errorf("acknowledgement source %q is not api or yaml", source)
	}
	if strings.TrimSpace(actor) == "" {
		return nil, errors.New("an acknowledgement needs an actor")
	}
	if !req.Exported {
		return nil, ErrNotExported
	}
	seen := map[string]bool{}
	var ids []string
	for _, id := range req.AcknowledgedResources {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, ErrNoResources
	}
	sort.Strings(ids)
	return ids, nil
}

func unknownIDs(inv Inventory, ids []string) []string {
	known := map[string]bool{}
	for _, it := range inv.Items {
		known[it.ID] = true
	}
	var missing []string
	for _, id := range ids {
		if !known[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

func ensureAckTable(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, `/* pg_sage agentdb_decommission v1 */
		SELECT to_regclass('sage.agentdb_decommission') IS NOT NULL`).
		Scan(&exists); err != nil {
		return fmt.Errorf("look up sage.agentdb_decommission: %w", err)
	}
	if !exists {
		return ErrAckTableMissing
	}
	return nil
}

func splitResult(ids, inserted []string) AckResult {
	fresh := map[string]bool{}
	for _, id := range inserted {
		fresh[id] = true
	}
	res := AckResult{Acknowledged: []string{}, AlreadyAcknowledged: []string{}}
	for _, id := range ids {
		if fresh[id] {
			res.Acknowledged = append(res.Acknowledged, id)
		} else {
			res.AlreadyAcknowledged = append(res.AlreadyAcknowledged, id)
		}
	}
	return res
}
