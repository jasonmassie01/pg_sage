package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/wal"
)

const defaultWALBackstopBytes int64 = 10 << 30

type DiskCapacityProvider interface {
	CapacityBytes(context.Context) (int64, error)
}

type PostgresWALOptions struct {
	AbandonAfter       time.Duration
	DiskPctCeiling     float64
	RetainedBytesLimit int64
	AllowDrop          bool
	DropOwnerAllowlist []string
	Disk               DiskCapacityProvider
}

type PostgresWALCustodian struct {
	pool     *pgxpool.Pool
	database string
	options  PostgresWALOptions
	mu       sync.Mutex
	inactive map[string]time.Time
}

func NewPostgresWALCustodian(
	pool *pgxpool.Pool, database string, options PostgresWALOptions,
) *PostgresWALCustodian {
	if options.AbandonAfter <= 0 {
		options.AbandonAfter = 24 * time.Hour
	}
	if options.DiskPctCeiling <= 0 {
		options.DiskPctCeiling = 10
	}
	if options.RetainedBytesLimit <= 0 {
		options.RetainedBytesLimit = defaultWALBackstopBytes
	}
	return &PostgresWALCustodian{
		pool: pool, database: database, options: options,
		inactive: make(map[string]time.Time),
	}
}

func (c *PostgresWALCustodian) Scan(ctx context.Context) ([]Proposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	diskBytes, diskKnown := c.diskCapacity(ctx)
	slots, err := c.readSlots(ctx)
	if err != nil {
		return nil, err
	}
	// max_slot_wal_keep_size is cluster-wide: size it for the slot that
	// retains the most WAL, and propose it at most once per scan.
	var maxRetained int64
	for _, slot := range slots {
		maxRetained = max(maxRetained, slot.retained)
	}
	result := make([]Proposal, 0)
	bounded := false
	for _, slot := range slots {
		proposal, err := c.proposalForSlot(ctx, slot, maxRetained, diskBytes, diskKnown)
		if err != nil {
			return nil, err
		}
		if proposal.SQL != "" && bounded {
			continue
		}
		bounded = bounded || proposal.SQL != ""
		if proposal.SQL != "" || proposal.Plan != "" {
			result = append(result, proposal)
		}
	}
	return result, nil
}

type walSlot struct {
	name, slotType string
	active         bool
	retained       int64
}

func (c *PostgresWALCustodian) readSlots(ctx context.Context) ([]walSlot, error) {
	rows, err := c.pool.Query(ctx, walSlotsSQL)
	if err != nil {
		return nil, fmt.Errorf("read replication slots: %w", err)
	}
	defer rows.Close()
	var slots []walSlot
	for rows.Next() {
		slot, err := scanWALSlot(rows)
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate replication slots: %w", err)
	}
	return slots, nil
}

func scanWALSlot(row interface{ Scan(...any) error }) (walSlot, error) {
	var slot walSlot
	if err := row.Scan(&slot.name, &slot.slotType, &slot.active, &slot.retained); err != nil {
		return slot, fmt.Errorf("scan replication slot: %w", err)
	}
	return slot, nil
}

func (c *PostgresWALCustodian) scanSlot(
	ctx context.Context, row interface{ Scan(...any) error }, diskBytes int64, diskKnown bool,
) (Proposal, error) {
	slot, err := scanWALSlot(row)
	if err != nil {
		return Proposal{}, err
	}
	return c.proposalForSlot(ctx, slot, slot.retained, diskBytes, diskKnown)
}

func (c *PostgresWALCustodian) proposalForSlot(
	ctx context.Context, slot walSlot, maxRetained, diskBytes int64, diskKnown bool,
) (Proposal, error) {
	evidence := c.slotEvidence(ctx, slot.name, wal.SlotType(slot.slotType),
		slot.active, slot.retained)
	evidence.DiskCapacityKnown, evidence.DiskCapacityBytes = diskKnown, diskBytes
	decision, err := wal.Classify(ctx, evidence, c.policy())
	if err != nil {
		return Proposal{}, err
	}
	switch decision.Action {
	case wal.ActionBound:
		return c.boundOrEscalate(ctx, slot, evidence, maxRetained)
	case wal.ActionDrop:
		return c.dropProposal(slot.name), nil
	default:
		return Proposal{}, nil
	}
}

// boundOrEscalate never proposes a bound that would invalidate a slot: the
// target keeps 50% headroom above the most WAL any slot retains. A
// registered consumer, or a target the disk cannot hold, is escalated as a
// plan-only proposal for a human instead.
func (c *PostgresWALCustodian) boundOrEscalate(
	ctx context.Context, slot walSlot, evidence wal.SlotEvidence, maxRetained int64,
) (Proposal, error) {
	target := walBoundTarget(maxRetained, c.options.RetainedBytesLimit)
	if evidence.ConsumerRegistered {
		return c.walEscalation(slot, target,
			"slot has a registered consumer; bounding WAL could invalidate it"), nil
	}
	if evidence.DiskCapacityKnown && target > evidence.DiskCapacityBytes*9/10 {
		return c.walEscalation(slot, target,
			"a safe max_slot_wal_keep_size exceeds disk capacity"), nil
	}
	return c.boundProposalIfNeeded(ctx, slot.name, target)
}

// walBoundTarget is max(limit, 1.5 x retained), rounded up to a megabyte.
func walBoundTarget(retained, limit int64) int64 {
	target := max(limit, retained+retained/2)
	const megabyte = int64(1 << 20)
	return (target + megabyte - 1) / megabyte * megabyte
}

func (c *PostgresWALCustodian) walEscalation(slot walSlot, target int64, reason string) Proposal {
	plan := "Escalate WAL retention on slot " + slot.name + ": " + reason
	return Proposal{
		Database: c.database, Feature: "wal", Plan: plan,
		TargetObjects: []string{"slot:" + slot.name},
		Evidence: map[string]any{
			"retained_wal_bytes": slot.retained, "safe_bound_bytes": target,
			"reason": reason, "plan": plan,
		},
	}
}

func (c *PostgresWALCustodian) boundProposalIfNeeded(
	ctx context.Context, slot string, target int64,
) (Proposal, error) {
	var setting int64
	var unit string
	if err := c.pool.QueryRow(ctx, `SELECT setting::bigint, COALESCE(unit,'')
		FROM pg_settings WHERE name='max_slot_wal_keep_size'`).Scan(&setting, &unit); err != nil {
		return Proposal{}, fmt.Errorf("read max_slot_wal_keep_size: %w", err)
	}
	currentBytes, err := postgresSettingBytes(setting, unit)
	if err != nil {
		return Proposal{}, err
	}
	if !needsWALBackstop(currentBytes, target) {
		return Proposal{}, nil
	}
	return c.boundProposal(slot, target), nil
}

func postgresSettingBytes(setting int64, unit string) (int64, error) {
	if setting < 0 {
		return setting, nil
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "b", "":
		return setting, nil
	case "kb":
		return setting << 10, nil
	case "mb":
		return setting << 20, nil
	case "gb":
		return setting << 30, nil
	default:
		return 0, fmt.Errorf("unsupported PostgreSQL size unit %q", unit)
	}
}

func needsWALBackstop(currentBytes, limit int64) bool {
	return currentBytes < 0 || currentBytes > limit
}

func (c *PostgresWALCustodian) slotEvidence(
	ctx context.Context, name string, slotType wal.SlotType, active bool, retained int64,
) wal.SlotEvidence {
	now := time.Now()
	c.mu.Lock()
	since, exists := c.inactive[name]
	if active {
		delete(c.inactive, name)
	} else if !exists {
		since, c.inactive[name] = now, now
	}
	c.mu.Unlock()
	evidence := wal.SlotEvidence{
		SlotName: name, SlotType: slotType, Active: active,
		LastActivityKnown: true, RetainedWALKnown: true, RetainedWALBytes: retained,
	}
	if !active {
		evidence.InactiveFor = now.Sub(since)
	}
	c.readRegistry(ctx, name, &evidence)
	return evidence
}

func (c *PostgresWALCustodian) readRegistry(
	ctx context.Context, name string, evidence *wal.SlotEvidence,
) {
	var owner string
	var registered bool
	err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT owner_tag, registered
		FROM sage.slot_consumer_registry WHERE slot_name=$1`, name).Scan(&owner, &registered)
	if errors.Is(err, pgx.ErrNoRows) {
		evidence.RegistryEvidenceKnown = true
		return
	}
	if err != nil {
		return
	}
	evidence.RegistryEvidenceKnown = true
	evidence.ConsumerRegistered = registered
	evidence.OwnerTagKnown = strings.TrimSpace(owner) != ""
	evidence.OwnerTag = owner
}

func (c *PostgresWALCustodian) diskCapacity(ctx context.Context) (int64, bool) {
	if c.options.Disk == nil {
		return 0, false
	}
	bytes, err := c.options.Disk.CapacityBytes(ctx)
	return bytes, err == nil && bytes > 0
}

func (c *PostgresWALCustodian) policy() wal.Policy {
	return wal.Policy{
		AbandonAfter:              c.options.AbandonAfter,
		RetainedWALBytesThreshold: c.options.RetainedBytesLimit,
		RetainedWALDiskPctCeiling: c.options.DiskPctCeiling,
		AllowDrop:                 c.options.AllowDrop,
		DropOwnerAllowlist:        append([]string(nil), c.options.DropOwnerAllowlist...),
	}
}

func (c *PostgresWALCustodian) boundProposal(slot string, targetBytes int64) Proposal {
	return Proposal{
		Database: c.database, Feature: "wal",
		SQL: fmt.Sprintf("ALTER SYSTEM SET max_slot_wal_keep_size = '%dMB'",
			targetBytes/(1<<20)),
		TargetObjects: []string{"slot:" + slot},
	}
}

func (c *PostgresWALCustodian) dropProposal(slot string) Proposal {
	quoted := strings.ReplaceAll(slot, "'", "''")
	return Proposal{
		Database: c.database, Feature: "wal",
		SQL:           "SELECT pg_drop_replication_slot('" + quoted + "')",
		TargetObjects: []string{"slot:" + slot},
	}
}

const walSlotsSQL = `/* pg_sage */ SELECT slot_name, slot_type, active,
COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn),0)::bigint
FROM pg_replication_slots`
