// Package auditchain makes pg_sage's audit tables tamper-evident (E2,
// spec §6.17): every audit row carries the hash of its predecessor, and a
// verifier detects edits, deletions and reordering.
//
// A chain is one audit table. Each write appends a link to
// sage.audit_chain_link: an insert (I), a change of the row's mutable
// state (U), a delete (D) or a truncate (T). A link hashes its predecessor's
// hash, its position, the operation, the row id, a hash of the row's sealed
// columns (what happened, never rewritten) and the row's state (outcome
// columns pg_sage legitimately updates later). The inserted row also
// carries its own link in chain_seq, chain_prev_hash and chain_hash.
//
// Links are appended by deferred constraint triggers, so the chain is
// linked at commit: the advisory lock that orders writers is the last lock
// a transaction takes, and is held only while it commits. That keeps
// concurrent writers deadlock-free and the chain gap-free (a rolled-back
// write never takes a position), and makes chain order commit order, which
// the SIEM exporter uses as its cursor.
//
// The chain proves integrity against anyone who edits rows without also
// recomputing every later hash. Someone who can rewrite the whole chain is
// caught by the off-box copy: the SIEM export carries each link's hash.
package auditchain

import (
	"fmt"
	"regexp"
	"strings"
)

// Spec describes one chained table. Column expressions are SQL over the
// row alias %[1]s; Sealed must start with the row id.
type Spec struct {
	Chain   string   // chain name, also the link table's chain key
	Table   string   // qualified table, sage.<name>
	Version int      // payload version recorded on every link
	Sealed  []string // columns fixed at insert
	State   []string // columns legitimately updated later; empty = append-only
	// Optional installs the chain only when the table exists (tables
	// another workstream creates).
	Optional bool
}

var (
	chainName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	tableName = regexp.MustCompile(`^sage\.[a-z][a-z0-9_]{0,50}$`)
)

func (s Spec) validate() error {
	switch {
	case !chainName.MatchString(s.Chain):
		return fmt.Errorf("auditchain: spec chain %q is not a lower-case identifier", s.Chain)
	case !tableName.MatchString(s.Table):
		return fmt.Errorf("auditchain: spec %s table %q is not sage.<name>", s.Chain, s.Table)
	case s.Version < 1:
		return fmt.Errorf("auditchain: spec %s version must be positive", s.Chain)
	case len(s.Sealed) == 0:
		return fmt.Errorf("auditchain: spec %s has no sealed columns", s.Chain)
	}
	return nil
}

// epochUS renders a timestamptz column as integer microseconds since the
// epoch: exact, and independent of the session's TimeZone and DateStyle.
func epochUS(col string) string {
	return "(extract(epoch FROM %[1]s." + col + ") * 1000000)::bigint"
}

func col(name string) string { return "%[1]s." + name }

// ActionLog chains sage.action_log. What was executed and approved is
// sealed; the outcome columns the executor, verifier and value ledger
// update afterwards are state.
var ActionLog = Spec{
	Chain: "action_log", Table: "sage.action_log", Version: 1,
	Sealed: []string{col("id"), epochUS("executed_at"), col("action_type"),
		col("finding_id"), col("sql_executed"), col("rollback_sql"), col("before_state"),
		col("approved_by"), epochUS("approved_at"), col("decision_id"), col("database_id")},
	State: []string{col("outcome"), col("rollback_reason"), epochUS("measured_at"),
		col("after_state"), col("verification_id"), col("justification"),
		col("toil_minutes_saved"), col("toil_model_version")},
}

// AuthAudit chains sign-ins and user administration (E1). Append-only.
var AuthAudit = Spec{
	Chain: "auth_audit", Table: "sage.auth_audit", Version: 1,
	Sealed: []string{col("id"), col("event"), col("actor_user_id"), col("target_user_id"),
		col("detail"), epochUS("created_at"), col("source_ip")},
}

// ConfigAudit chains configuration changes. Append-only.
var ConfigAudit = Spec{
	Chain: "config_audit", Table: "sage.config_audit", Version: 1,
	Sealed: []string{col("id"), col("key"), col("old_value"), col("new_value"),
		col("database_id"), col("changed_by"), epochUS("changed_at"),
		col("changed_by_actor")},
}

// GuardQueryAudit chains brokered agent queries (§7). The table belongs to
// the core workstream, so the chain installs once it exists.
var GuardQueryAudit = Spec{
	Chain: "guard_query_audit", Table: "sage.guard_query_audit", Version: 1,
	Sealed: []string{col("id"), col("database_id") + "::text", col("principal_id"),
		col("task_id"), col("envelope_hash"), col("verdict"), col("reason"),
		col("row_count"), col("classes"), epochUS("at")},
	Optional: true,
}

// PGAuditEvents chains the pgaudit records pg_sage correlated to its own
// actions and to agent principals (§6.17). Append-only.
var PGAuditEvents = Spec{
	Chain: "pgaudit_events", Table: "sage.guard_pgaudit_events", Version: 1,
	Sealed: []string{col("id"), col("database_name"), epochUS("logged_at"),
		col("session_id"), col("pid"), col("db_user"), col("application"),
		col("principal_id"), col("action_id"), col("audit_type"), col("statement_id"),
		col("substatement_id"), col("class"), col("command"), col("object_type"),
		col("object_name"), col("statement"), col("correlated_by")},
}

// Specs lists the chains the schema migration installs.
func Specs() []Spec {
	return []Spec{ActionLog, AuthAudit, ConfigAudit, GuardQueryAudit, PGAuditEvents}
}

// SpecFor returns the built-in spec of chain.
func SpecFor(chain string) (Spec, bool) {
	for _, s := range Specs() {
		if s.Chain == chain {
			return s, true
		}
	}
	return Spec{}, false
}

// sealedSQL is the canonical text of the sealed columns of row alias.
func (s Spec) sealedSQL(alias string) string {
	return "jsonb_build_array(" + fmt.Sprintf(strings.Join(s.Sealed, ", "), alias) +
		")::text"
}

// sealedHashSQL is the sha256 hex of sealedSQL.
func (s Spec) sealedHashSQL(alias string) string {
	return "encode(sha256(convert_to(" + s.sealedSQL(alias) + ", 'UTF8')), 'hex')"
}

// stateSQL is the canonical text of the state columns, empty when none.
func (s Spec) stateSQL(alias string) string {
	if len(s.State) == 0 {
		return "''"
	}
	return "jsonb_build_array(" + fmt.Sprintf(strings.Join(s.State, ", "), alias) +
		")::text"
}

// name is the table name without the schema.
func (s Spec) name() string { return strings.TrimPrefix(s.Table, "sage.") }
