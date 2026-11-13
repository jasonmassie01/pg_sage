package plan

import (
	"context"
	"fmt"
	"time"
)

// The online column swap (migration-09b) changes a column's type without
// the table rewrite ALTER COLUMN ... TYPE takes under ACCESS EXCLUSIVE: a
// new column kept in sync by a trigger, a batched backfill, one short
// swap transaction, then the keys, sequence and foreign keys moved over.
// The plan is recommend-only: pg_sage never runs it.

// Swap bounds.
const (
	DefaultSwapBatchSize   = 5000
	MaxSwapBatchSize       = 100000
	DefaultSwapLockTimeout = 2 * time.Second
	MinSwapLockTimeout     = time.Millisecond
	MaxSwapLockTimeout     = time.Minute
)

// LockNone is a step that takes no table lock (SET, BEGIN, COMMIT,
// CREATE FUNCTION).
const LockNone = "NONE"

// SwapPhase orders the steps; phases never go backwards.
type SwapPhase string

const (
	SwapPhasePrepare  SwapPhase = "prepare"  // new column, sync trigger
	SwapPhaseBackfill SwapPhase = "backfill" // batched copy
	SwapPhaseVerify   SwapPhase = "verify"   // NOT NULL proof, unique index
	SwapPhaseSwap     SwapPhase = "swap"     // the one short transaction
	SwapPhaseFinish   SwapPhase = "finish"   // validate keys, drop the trigger function
	SwapPhaseContract SwapPhase = "contract" // drop the old column (destructive)
)

// SwapRequest names the column and its new type. Names are catalog
// names (already folded); TargetType is one of the supported types.
type SwapRequest struct {
	Schema, Table, Column string
	TargetType            string
	BatchSize             int           // 0 = DefaultSwapBatchSize
	LockTimeout           time.Duration // 0 = DefaultSwapLockTimeout
}

// SwapFacts is what the catalog says about the table and column
// (ReadSwapFacts). The planner is a pure function of facts and request.
type SwapFacts struct {
	Schema, Table  string
	RelKind        string // pg_class.relkind
	IsPartition    bool
	HasInheritance bool // has an inheritance parent or child
	EstimatedRows  int64
	ServerVersion  int // server_version_num
	Column         SwapColumn
	PrimaryKey     *SwapKey // nil without a primary key
	Referencing    []SwapForeignKey
	Outgoing       []string // foreign keys on the column itself
	Dependents     []string // other objects that depend on the column
}

// SwapColumn is the column being changed.
type SwapColumn struct {
	Name, Type     string // Type is format_type output
	NotNull        bool
	Identity       bool
	Generated      bool
	Default        string // pg_get_expr of the default, "" for none
	DefaultIsSeq   bool   // the default is nextval() of the owned sequence
	SequenceSchema string // the sequence the column owns, if any
	SequenceName   string
	SequenceType   string
}

// SwapKey is the table's primary key.
type SwapKey struct {
	Constraint string
	Columns    []string
	ColumnType string // type of the first key column
	// Shapes the swap cannot rebuild on the new column's index.
	Deferrable      bool // DEFERRABLE
	Include         bool // the index has INCLUDE columns
	ReplicaIdentity bool // the index is the table's replica identity
}

// SwapForeignKey references the column from another (or the same) table.
type SwapForeignKey struct {
	Constraint, Schema, Table string
	Column, ColumnType        string // the referencing column
	Columns                   int    // number of key columns
	OnUpdate, OnDelete        string // pg_constraint confupdtype / confdeltype
	Match                     string // confmatchtype
	Deferrable, Deferred      bool
	SetColumns                bool // ON DELETE SET NULL/DEFAULT (col, ...)
	ChildPartitioned          bool
	ChildIsPartition          bool
	NotValidated              bool // NOT VALID, or NOT ENFORCED (PostgreSQL 18)
}

// SwapStep is one statement of the script.
type SwapStep struct {
	Name             string
	Phase            SwapPhase
	SQL              string
	LockLevel        string // the lock the statement takes; LockNone for none
	InTransaction    bool   // part of the swap transaction (BEGIN..COMMIT)
	Repeat           bool   // run with $1 = last key returned until no rows return
	DML              bool   // a data change: outside the DDL classifier
	RequiresTopLevel bool   // cannot run inside a transaction block
	Destructive      bool
	Note             string
	// Known and Rules are the DDL classifier's view of SQL.
	Known bool
	Rules []string
}

// SwapPlan is the recommend-only script.
type SwapPlan struct {
	Schema, Table, Column string
	FromType, ToType      string
	EstimatedRows         int64
	RecommendOnly         bool
	// Degraded: the build has no SQL parser, so the classifier could not
	// confirm the forms the regex rules do not cover.
	Degraded bool
	Steps    []SwapStep
}

// PlanColumnSwap returns the online swap for req, or an error naming why
// the column is out of scope. It reads nothing: facts come from
// ReadSwapFacts.
func PlanColumnSwap(ctx context.Context, facts SwapFacts, req SwapRequest) (SwapPlan, error) {
	if err := ctx.Err(); err != nil {
		return SwapPlan{}, fmt.Errorf("plan column swap: %w", err)
	}
	req, err := normalizeSwapRequest(req)
	if err != nil {
		return SwapPlan{}, err
	}
	if err := checkSwapFacts(facts, req); err != nil {
		return SwapPlan{}, fmt.Errorf("column swap of %s.%s.%s refused: %w",
			req.Schema, req.Table, req.Column, err)
	}
	names := newSwapNames(facts, req)
	if err := names.check(); err != nil {
		return SwapPlan{}, err
	}
	plan := SwapPlan{Schema: facts.Schema, Table: facts.Table, Column: facts.Column.Name,
		FromType: facts.Column.Type, ToType: req.TargetType,
		EstimatedRows: facts.EstimatedRows, RecommendOnly: true,
		Steps: buildSwapSteps(facts, req, names)}
	if err := classifySwapSteps(ctx, &plan, facts.ServerVersion); err != nil {
		return SwapPlan{}, err
	}
	return plan, nil
}

// normalizeSwapRequest applies the defaults and refuses out-of-range
// bounds.
func normalizeSwapRequest(req SwapRequest) (SwapRequest, error) {
	if req.Schema == "" || req.Table == "" || req.Column == "" {
		return req, fmt.Errorf("column swap needs a schema, table and column; got an empty name")
	}
	if !swapTargetTypes[req.TargetType] {
		return req, fmt.Errorf("column swap target type %q is not supported; use one of %s",
			req.TargetType, swapTargetList())
	}
	switch {
	case req.BatchSize == 0:
		req.BatchSize = DefaultSwapBatchSize
	case req.BatchSize < 0 || req.BatchSize > MaxSwapBatchSize:
		return req, fmt.Errorf("column swap batch size %d is outside 1..%d",
			req.BatchSize, MaxSwapBatchSize)
	}
	switch {
	case req.LockTimeout == 0:
		req.LockTimeout = DefaultSwapLockTimeout
	case req.LockTimeout < MinSwapLockTimeout || req.LockTimeout > MaxSwapLockTimeout:
		return req, fmt.Errorf("column swap lock_timeout %v is outside %v..%v",
			req.LockTimeout, MinSwapLockTimeout, MaxSwapLockTimeout)
	}
	return req, nil
}
