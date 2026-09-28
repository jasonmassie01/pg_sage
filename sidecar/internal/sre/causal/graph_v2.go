package causal

import "github.com/pg-sage/sidecar/internal/sre/probes"

// Graph v2 nodes (Sage SRE M2): connection pressure, WAL/slot retention
// and pg_sage's own changes (AI-SRE-SPEC §6 table).
const (
	PoolFanOut      NodeID = "pool_fan_out"
	BlockedBacklog  NodeID = "blocked_backlog"
	ConnectionLeak  NodeID = "connection_leak"
	InactiveSlot    NodeID = "inactive_slot"
	SlowConsumer    NodeID = "slow_consumer"
	ArchiverFailure NodeID = "archiver_failure"
	WriteSurge      NodeID = "write_surge"
	SageOwnAction   NodeID = "sage_own_action"
)

var v2Nodes = append(connectionNodes, append(walNodes, changeNodes...)...)

var connectionNodes = []Node{
	{ID: PoolFanOut, Family: FamilyConnections, Label: "pool fan-out",
		Mechanism: "Each application instance keeps its own pool open, so idle " +
			"connections grow with the number of instances.",
		Predicted:   "many idle backends from one application, stable over time, no lock waits",
		Refutation:  string(probes.ConnectionSaturation),
		Confounders: "a traffic increase",
		OperatorStep: "Lower each instance's pool size or put a shared pooler in front; " +
			"raising max_connections only hides it."},
	{ID: BlockedBacklog, Family: FamilyConnections, Label: "blocked-query backlog",
		Mechanism:  "Backends pile up waiting on a lock, holding their connections.",
		Predicted:  "many active backends waiting on locks (wait_event_type Lock)",
		Refutation: string(probes.LockGraph), Confounders: "slow queries that are not blocked",
		OperatorStep: "Resolve the lock chain the backends wait on; the connections " +
			"return when it clears."},
	{ID: ConnectionLeak, Family: FamilyConnections, Label: "connection leak",
		Mechanism:  "An application opens connections and never returns them.",
		Predicted:  "idle backends of one application growing steadily between samples",
		Refutation: string(probes.ConnectionSaturation), Confounders: "autoscaling",
		OperatorStep: "Find the code path that opens connections without closing them; " +
			"restart or roll back that application through your normal process."},
}

var walNodes = []Node{
	{ID: InactiveSlot, Family: FamilyWAL, Label: "inactive replication slot",
		Mechanism:  "A slot without a consumer holds back WAL removal.",
		Predicted:  "slot inactive, retained WAL large and growing",
		Refutation: string(probes.ReplicationSlots), Confounders: "a write surge",
		OperatorStep: "Restore the slot's consumer, or with its owner's agreement drop the " +
			"slot (a reviewed manual step: the consumer loses its position)."},
	{ID: SlowConsumer, Family: FamilyWAL, Label: "slow slot consumer",
		Mechanism:  "A connected consumer confirms WAL slower than it is written.",
		Predicted:  "slot active, retained WAL growing between samples",
		Refutation: string(probes.ReplicationSlots), Confounders: "the network",
		OperatorStep: "Check the consumer's throughput and network; it is connected but " +
			"falling behind."},
	{ID: ArchiverFailure, Family: FamilyWAL, Label: "WAL archiver failure",
		Mechanism:  "archive_command fails, so WAL segments cannot be recycled.",
		Predicted:  "last archive attempt failed, failed_count rising",
		Refutation: string(probes.Archiver), Confounders: "replication slots",
		OperatorStep: "Fix archive_command (the server log names the failing command); " +
			"WAL is kept until archiving succeeds."},
	{ID: WriteSurge, Family: FamilyWAL, Label: "write surge",
		Mechanism:  "WAL is written much faster than usual.",
		Predicted:  "WAL rate a step above its long-run average",
		Refutation: string(probes.WALCheckpoint), Confounders: "an expected bulk load",
		Amplifies: []NodeID{InactiveSlot, SlowConsumer},
		OperatorStep: "Identify the bulk write (load, migration or batch job); an expected " +
			"surge needs only disk headroom."},
}

var changeNodes = []Node{
	{ID: SageOwnAction, Family: FamilyChange, Label: "pg_sage's own change",
		Mechanism: "An action pg_sage took shortly before the incident caused or " +
			"worsened it.",
		Predicted:   "a pg_sage action in the window before the incident",
		Refutation:  string(probes.SageActions),
		Confounders: "any other change in the same window",
		OperatorStep: "Review pg_sage's action in the window and roll it back from the " +
			"Actions page if it is implicated."},
}
