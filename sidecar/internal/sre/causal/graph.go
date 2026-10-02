// Package causal is the Sage SRE causal graph v1 (AI-SRE-SPEC §6): a
// hand-built, versioned model of PostgreSQL incident mechanisms. Nodes
// are mechanisms; each has the observations that support it, a
// discriminating refutation probe from the catalog, and the nodes it
// amplifies. Matching is deterministic: hypotheses are scored from
// typed probe evidence, contradicted ones are ruled out with the
// evidence that contradicts them, and no LLM is involved.
package causal

import "github.com/pg-sage/sidecar/internal/sre/probes"

// GraphVersion pins the graph a diagnosis was produced with.
const GraphVersion = "causal-v3"

// NoRefutation marks a hypothesis without a discriminating probe.
const NoRefutation = "none_available"

// SupportThreshold is the confidence at or above which a hypothesis is
// supported. Below it a hypothesis stays an unproven alternative.
const SupportThreshold = 0.5

// Family groups nodes by incident family.
type Family string

// Families modeled by the graph (v1: lock, plan; v2 adds connection
// pressure, WAL retention and pg_sage's own changes; v3's M6 families
// are in graph_m6.go).
const (
	FamilyLockBlocking   Family = "lock_blocking"
	FamilyPlanRegression Family = "plan_regression"
	FamilyConnections    Family = "connection_pressure"
	FamilyWAL            Family = "wal_retention"
	FamilyChange         Family = "change"
)

// NodeID names a mechanism.
type NodeID string

// v1 nodes, in graph order (the tie-break order).
const (
	IdleInTxHolder            NodeID = "idle_in_tx_holder"
	PreparedXactHolder        NodeID = "prepared_xact_holder"
	DDLLockQueue              NodeID = "ddl_lock_queue"
	HotRowContention          NodeID = "hot_row_contention"
	PlanFlipRegression        NodeID = "plan_flip_regression"
	SamePlanLatencyRegression NodeID = "same_plan_latency_regression"
)

// Node is one mechanism of the graph.
type Node struct {
	ID          NodeID
	Family      Family
	Label       string // short human label
	Mechanism   string
	Predicted   string // observations that support it
	Refutation  string // catalog probe id, or NoRefutation
	Confounders string
	Amplifies   []NodeID // when both are supported, this one contributes
	// OperatorStep is the manual next step for an operator (never an
	// action pg_sage executes).
	OperatorStep string
}

var graph = append(append(v1Nodes, v2Nodes...), m6Nodes...)

var v1Nodes = []Node{
	{ID: IdleInTxHolder, Family: FamilyLockBlocking,
		Label:       "idle-in-transaction holder",
		Mechanism:   "An idle-in-transaction session holds locks others wait for.",
		Predicted:   "chain head state 'idle in transaction', old transaction, waiters",
		Refutation:  string(probes.LockGraph),
		Confounders: "a long analytics query that is active, not idle",
		OperatorStep: "End the transaction from its application (commit or roll back), " +
			"or terminate the session through your normal process after checking its " +
			"work. pg_cancel_backend does not end an idle transaction: it has no " +
			"running query to cancel."},
	{ID: PreparedXactHolder, Family: FamilyLockBlocking,
		Label:       "prepared-transaction holder",
		Mechanism:   "A prepared (two-phase) transaction holds locks others wait for.",
		Predicted:   "blocker pid 0 in the lock graph, an old pg_prepared_xacts row",
		Refutation:  string(probes.PreparedXacts),
		Confounders: "none known",
		OperatorStep: "Resolve the prepared transaction with its transaction manager " +
			"(COMMIT PREPARED or ROLLBACK PREPARED) after confirming its outcome with " +
			"the owning application."},
	{ID: DDLLockQueue, Family: FamilyLockBlocking,
		Label: "DDL queued behind a long transaction",
		Mechanism: "A DDL-strength lock request queues behind an open transaction; " +
			"every later reader queues behind the DDL (lock-queue amplification).",
		Predicted: "an ACCESS EXCLUSIVE (or other DDL-strength) waiter on the head, " +
			"waiters behind it",
		Refutation:  string(probes.LockGraph),
		Confounders: "the migration itself is long-running rather than waiting",
		Amplifies:   []NodeID{IdleInTxHolder, PreparedXactHolder},
		OperatorStep: "Stop or reschedule the queued DDL with a lock_timeout, then " +
			"deal with the transaction it waits behind."},
	{ID: HotRowContention, Family: FamilyLockBlocking,
		Label:       "hot-row contention",
		Mechanism:   "Short transactions update the same rows and wait on each other's row locks.",
		Predicted:   "several transactionid/tuple waits, an active head with a short transaction",
		Refutation:  string(probes.LockGraph),
		Confounders: "foreign-key check contention",
		OperatorStep: "Reduce concurrent updates of the same rows in the application " +
			"(batching, queueing or shorter transactions)."},
	{ID: PlanFlipRegression, Family: FamilyPlanRegression,
		Label:       "plan flip regression",
		Mechanism:   "The query's plan changed and the new plan is slower.",
		Predicted:   "plan_hash changed at the regression point, latency ratio >= 1.5",
		Refutation:  string(probes.PlanRegressions),
		Confounders: "data growth or contention coinciding with the plan change",
		OperatorStep: "Compare the old and new plans; refresh statistics or pin the " +
			"good plan through a reviewed query hint."},
	{ID: SamePlanLatencyRegression, Family: FamilyPlanRegression,
		Label: "latency regression without a plan change",
		Mechanism: "The query slowed down on the same plan (data growth, " +
			"contention, cache or I/O).",
		Predicted:   "plan_hash unchanged across the window, latency ratio >= 1.5",
		Refutation:  string(probes.PlanRegressions),
		Confounders: "a plan change that was not captured",
		OperatorStep: "Check data growth, contention and cache or I/O pressure for " +
			"the query; its plan did not change."},
}

// Graph returns a copy of the graph.
func Graph() []Node {
	out := make([]Node, len(graph))
	for i, n := range graph {
		n.Amplifies = append([]NodeID(nil), n.Amplifies...)
		out[i] = n
	}
	return out
}

// NodeByID returns one node.
func NodeByID(id NodeID) (Node, bool) {
	for _, n := range graph {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func graphIndex(id NodeID) int {
	for i, n := range graph {
		if n.ID == id {
			return i
		}
	}
	return len(graph)
}
