package sqlast

// Statement kinds InspectStatements reports. Any other statement is
// reported as KindOther with its parse-node name in Node.
const (
	KindAlterTable  = "alter_table"
	KindRename      = "rename"
	KindCreateIndex = "create_index"
	KindUpdate      = "update"
	KindAnalyze     = "analyze"
	KindSet         = "set"
	KindReset       = "reset"
	KindBegin       = "begin"
	KindCommit      = "commit"
	KindOther       = "other"
)

// Statement is one statement of a script, as the parser sees it. It
// describes; callers decide what to accept.
type Statement struct {
	// Kind is one of the Kind constants.
	Kind string
	// Node is the parse-node name (e.g. "DropStmt"), always set.
	Node string
	// Relations are every relation the statement names, with references
	// to the CTEs of its own top-level WITH left out where they are in
	// scope; the UPDATE target is always kept.
	Relations []QualifiedName
	// Functions are every function the statement calls.
	Functions []QualifiedName
	// AttributeCalls are x.name references: PostgreSQL runs x.name as
	// name(x) when x has no column called name, a call with no FuncCall.
	AttributeCalls []QualifiedName
	// Setting is the parameter a SET or RESET names; "" for RESET ALL
	// and SET TRANSACTION.
	Setting string
	// Commands are the ALTER TABLE subcommand types (e.g.
	// "AT_AddColumn"), or for a rename the renamed object type (e.g.
	// "OBJECT_COLUMN").
	Commands []string
	// TableTarget is false for ALTER/RENAME on something other than a
	// table (an index, a view, a sequence).
	TableTarget bool
	// NestedWrite is set when a data-modifying statement appears inside
	// this one (a writing common table expression).
	NestedWrite bool
	// Concurrent is set for CREATE INDEX CONCURRENTLY, which PostgreSQL
	// refuses inside a transaction block.
	Concurrent bool
}
