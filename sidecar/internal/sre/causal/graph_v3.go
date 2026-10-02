package causal

import "github.com/pg-sage/sidecar/internal/sre/probes"

// Graph v3 (Sage SRE M6): the runway families. A runway is time left
// before a hard limit: wraparound (what keeps tables from freezing), disk
// and WAL (what fills them; slot, archiver and surge nodes are shared
// with WAL retention) and sequence exhaustion (which limit binds).
const (
	FamilyWraparound Family = "wraparound_runway"
	FamilyDiskWAL    Family = "disk_wal_runway"
	FamilySequence   Family = "sequence_runway"
)

// Runway nodes.
const (
	XminHeldBySession          NodeID = "xmin_held_by_session"
	XminHeldByPreparedXact     NodeID = "xmin_held_by_prepared_xact"
	XminHeldByReplication      NodeID = "xmin_held_by_replication"
	AutovacuumSaturated        NodeID = "autovacuum_saturated"
	AutovacuumDisabled         NodeID = "autovacuum_disabled"
	AutovacuumCancelled        NodeID = "autovacuum_cancelled"
	XIDConsumptionSurge        NodeID = "xid_consumption_surge"
	DatabaseGrowth             NodeID = "database_growth"
	SequenceTypeLimit          NodeID = "sequence_type_limit"
	ColumnNarrowerThanSequence NodeID = "column_narrower_than_sequence"
	ExplicitMaxvalueLimit      NodeID = "explicit_maxvalue_limit"
)

var v3Nodes = append(append(append([]Node(nil), wraparoundNodes...),
	diskWALNodes...), sequenceNodes...)

var wraparoundNodes = []Node{
	{ID: XminHeldBySession, Family: FamilyWraparound, Label: "session holds the xmin horizon",
		Mechanism: "An open transaction's snapshot or XID keeps vacuum from freezing " +
			"rows newer than it, so table ages keep rising.",
		Predicted:   "a session whose xmin age is close to the oldest table's XID age",
		Refutation:  string(probes.XminHorizon),
		Confounders: "a long but young transaction that started after the table's horizon",
		OperatorStep: "End the transaction that holds the horizon from its application " +
			"(commit or roll back), or terminate the session through your normal process " +
			"after checking its work; vacuum can then freeze past it."},
	{ID: XminHeldByPreparedXact, Family: FamilyWraparound,
		Label:      "prepared transaction holds the xmin horizon",
		Mechanism:  "A forgotten two-phase transaction holds its XID and the horizon.",
		Predicted:  "a prepared transaction whose age is close to the oldest table's XID age",
		Refutation: string(probes.XminHorizon), Confounders: "none known",
		OperatorStep: "Resolve the prepared transaction with its transaction manager " +
			"(COMMIT PREPARED or ROLLBACK PREPARED) after confirming its outcome."},
	{ID: XminHeldByReplication, Family: FamilyWraparound,
		Label: "replication slot or standby holds the xmin horizon",
		Mechanism: "A slot's xmin or catalog_xmin, or a standby's feedback, holds the " +
			"horizon for its consumer.",
		Predicted:   "a slot or standby whose xmin age is close to the oldest table's XID age",
		Refutation:  string(probes.XminHorizon),
		Confounders: "a slot that is active and advancing",
		OperatorStep: "Restore or advance the consumer (or the standby's feedback); with " +
			"its owner's agreement drop an abandoned slot (a reviewed manual step: the " +
			"consumer loses its position)."},
	{ID: AutovacuumSaturated, Family: FamilyWraparound, Label: "autovacuum saturated",
		Mechanism:   "Every autovacuum worker is busy, so the oldest tables wait their turn.",
		Predicted:   "all autovacuum workers busy in every sample",
		Refutation:  string(probes.XIDRunwayProbe),
		Confounders: "workers busy on the very tables that need freezing",
		OperatorStep: "Give autovacuum capacity (more workers or a higher cost limit, a " +
			"reviewed setting change) or VACUUM (FREEZE) the oldest tables in a quiet " +
			"window; pg_sage's freeze custodian proposes the freeze through the policy gate."},
	{ID: AutovacuumDisabled, Family: FamilyWraparound, Label: "autovacuum disabled",
		Mechanism: "Autovacuum is off (globally or for the table), so tables are only " +
			"frozen by the forced anti-wraparound vacuum at their maximum.",
		Predicted:   "autovacuum off, or autovacuum_enabled = false on the table",
		Refutation:  string(probes.WraparoundTablesProbe),
		Confounders: "a table excluded on purpose and frozen by a scheduled job",
		OperatorStep: "Re-enable autovacuum for the table (or globally), or schedule " +
			"VACUUM (FREEZE) for it before its maximum."},
	{ID: AutovacuumCancelled, Family: FamilyWraparound, Label: "autovacuum cancelled",
		Mechanism: "Conflicting lock requests keep cancelling autovacuum before it " +
			"freezes the table.",
		Predicted:   "logged 'canceling autovacuum task' incidents in the window",
		Refutation:  string(probes.AutovacuumCancellations),
		Confounders: "no log source configured (cancellations unseen)",
		OperatorStep: "Find what keeps taking conflicting locks on the table (frequent " +
			"DDL, LOCK TABLE, long lock waits) and give autovacuum a window."},
	{ID: XIDConsumptionSurge, Family: FamilyWraparound, Label: "XID consumption surge",
		Mechanism:   "Transaction IDs are consumed much faster than usual.",
		Predicted:   "XID rate between samples several times its measured trend",
		Refutation:  string(probes.RunwayTrendsProbe),
		Confounders: "an expected batch load",
		Amplifies: []NodeID{XminHeldBySession, XminHeldByPreparedXact,
			XminHeldByReplication, AutovacuumSaturated, AutovacuumDisabled,
			AutovacuumCancelled},
		OperatorStep: "Identify the workload consuming XIDs (single-row commits, " +
			"savepoints, retried transactions); it shortens the runway another " +
			"mechanism holds back."},
}

var diskWALNodes = []Node{
	{ID: DatabaseGrowth, Family: FamilyDiskWAL, Label: "database growth",
		Mechanism:   "The databases' own files grow steadily toward the disk's capacity.",
		Predicted:   "database size rising steadily across the sampled window",
		Refutation:  string(probes.RunwayTrendsProbe),
		Confounders: "a bulk load followed by a delete (churn, not growth)",
		OperatorStep: "Find the fastest-growing tables and review retention, " +
			"partitioning or archival; plan storage growth before the projected date."},
}

var sequenceNodes = []Node{
	{ID: SequenceTypeLimit, Family: FamilySequence, Label: "sequence type limit",
		Mechanism:   "The sequence consumes values toward the maximum of its own type.",
		Predicted:   "the sequence's maximum is its type's, and the owning column is as wide",
		Refutation:  string(probes.SequenceRunwayProbe),
		Confounders: "a dormant or cycling sequence",
		OperatorStep: "Plan the move to bigint: ALTER SEQUENCE ... AS bigint and the " +
			"owning column to bigint (a table rewrite: a reviewed manual migration)."},
	{ID: ColumnNarrowerThanSequence, Family: FamilySequence,
		Label: "owning column narrower than its sequence",
		Mechanism: "The sequence has room, but the integer column it fills overflows " +
			"first and inserts fail.",
		Predicted:   "the owning column's type maximum is below the sequence's maximum",
		Refutation:  string(probes.SequenceRunwayProbe),
		Confounders: "a sequence shared with a wider column",
		OperatorStep: "Widen the owning column to the sequence's type (ALTER TABLE ... " +
			"ALTER COLUMN ... TYPE bigint: a table rewrite, a reviewed manual migration)."},
	{ID: ExplicitMaxvalueLimit, Family: FamilySequence, Label: "explicit MAXVALUE",
		Mechanism:   "An explicit MAXVALUE below the sequence's type is reached first.",
		Predicted:   "the sequence's maximum is below its type's maximum",
		Refutation:  string(probes.SequenceRunwayProbe),
		Confounders: "a cap that is intended (the application wraps or retires it)",
		OperatorStep: "Raise MAXVALUE (ALTER SEQUENCE ... MAXVALUE, no rewrite) after " +
			"confirming why it was capped."},
}
