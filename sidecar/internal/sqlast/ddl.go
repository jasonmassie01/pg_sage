package sqlast

// DDLKind classifies one statement, or one part of a statement, of a
// migration script.
type DDLKind string

// The DDL kinds InspectDDL reports. Everything else is DDLOther.
const (
	DDLCreateIndex        DDLKind = "create_index"
	DDLDropIndex          DDLKind = "drop_index"
	DDLCreateTable        DDLKind = "create_table"
	DDLDropTable          DDLKind = "drop_table"
	DDLRenameTable        DDLKind = "rename_table"
	DDLRenameColumn       DDLKind = "rename_column"
	DDLAddColumn          DDLKind = "add_column"
	DDLDropColumn         DDLKind = "drop_column"
	DDLAlterType          DDLKind = "alter_type"
	DDLSetNotNull         DDLKind = "set_not_null"
	DDLDropNotNull        DDLKind = "drop_not_null"
	DDLSetDefault         DDLKind = "set_default"
	DDLAddConstraint      DDLKind = "add_constraint"
	DDLValidateConstraint DDLKind = "validate_constraint"
	DDLDropConstraint     DDLKind = "drop_constraint"
	DDLCreateStatistics   DDLKind = "create_statistics"
	DDLUpdate             DDLKind = "dml_update"
	DDLDelete             DDLKind = "dml_delete"
	DDLInsert             DDLKind = "dml_insert"
	DDLTransaction        DDLKind = "transaction"
	DDLSet                DDLKind = "set"
	DDLOther              DDLKind = "other"
)

// DDLStatement is one typed entry of a script. A statement that does
// several things (ALTER TABLE with several subcommands, DROP of several
// objects) yields one entry per part; the parts share Statement, Offset,
// Line and Text and are numbered by Part.
type DDLStatement struct {
	Kind DDLKind `json:"kind"`
	// Command is the parse-tree node of the statement, e.g. "AlterTableStmt",
	// "IndexStmt", "SelectStmt".
	Command string `json:"command"`
	// Statement is the 0-based ordinal of the SQL statement in the script.
	Statement int `json:"statement"`
	// Part is the 0-based subcommand or object index within the statement.
	Part int `json:"part,omitempty"`
	// Offset is the byte offset of the statement's first token in the script
	// (leading whitespace and comments skipped); Line is its 1-based line.
	Offset int `json:"offset"`
	Line   int `json:"line"`
	// Text is the statement without leading whitespace and comments, trailing
	// whitespace or the terminating semicolon: script[Offset:Offset+len(Text)].
	Text string `json:"text"`
	// Relation is the table the statement works on (the created, altered,
	// dropped or written table or view). It is empty for drop_index, whose
	// table the SQL does not name.
	Relation QualifiedName `json:"relation"`
	// Object is the non-table object created or dropped: the index of
	// create_index (in the table's schema) and drop_index, or the statistics
	// object of create_statistics. Empty when the SQL names none.
	Object QualifiedName `json:"object"`
	// ObjectType is "table", "view", "materialized_view" or "foreign_table"
	// for drop_table and rename_table.
	ObjectType string `json:"object_type,omitempty"`
	// Columns are the plain column names the statement names: the touched
	// column, constraint columns, index key and INCLUDE columns, table
	// columns, statistics columns, UPDATE SET targets or INSERT columns.
	Columns []string `json:"columns,omitempty"`
	// NewName is the new name of rename_table and rename_column.
	NewName    string         `json:"new_name,omitempty"`
	Index      *IndexDef      `json:"index,omitempty"`
	Constraint *ConstraintDef `json:"constraint,omitempty"`
	Column     *ColumnDef     `json:"column,omitempty"`
	Table      *TableDef      `json:"table,omitempty"`
	Setting    *Setting       `json:"setting,omitempty"`
	// Transaction is the transaction command: begin, start, commit, rollback,
	// savepoint, release, rollback_to, prepare, commit_prepared or
	// rollback_prepared.
	Transaction string `json:"transaction,omitempty"`
	// Subcommand is the ALTER TABLE subcommand (e.g. "AT_AddColumn",
	// "AT_SetRelOptions") of every ALTER TABLE part.
	Subcommand string `json:"subcommand,omitempty"`
	// Behavior is "restrict" or "cascade" for drop_index, drop_table,
	// drop_column and drop_constraint.
	Behavior    string `json:"behavior,omitempty"`
	IfExists    bool   `json:"if_exists,omitempty"`
	IfNotExists bool   `json:"if_not_exists,omitempty"`
	// HasWhere is set for dml_update and dml_delete with a WHERE clause.
	HasWhere bool `json:"has_where,omitempty"`
	// InTransactionBlock is the explicit transaction state when the
	// statement starts: true after BEGIN or START TRANSACTION until COMMIT,
	// ROLLBACK or PREPARE TRANSACTION. BEGIN itself is false, COMMIT true.
	InTransactionBlock bool `json:"in_transaction_block,omitempty"`
	// LockTimeoutMs and StatementTimeoutMs are the values the script has set
	// (SET, SET LOCAL inside a block) in effect when the statement starts, in
	// milliseconds; 0 means disabled. Nil means the script has not set it (or
	// RESET it), so the server or role default applies.
	LockTimeoutMs      *int64 `json:"lock_timeout_ms,omitempty"`
	StatementTimeoutMs *int64 `json:"statement_timeout_ms,omitempty"`
}

// IndexDef describes create_index and drop_index (Name and Concurrent only).
type IndexDef struct {
	Name        string `json:"name,omitempty"`
	Unique      bool   `json:"unique,omitempty"`
	Concurrent  bool   `json:"concurrent,omitempty"`
	IfNotExists bool   `json:"if_not_exists,omitempty"`
	// Method is the access method ("btree" when the SQL names none).
	Method string `json:"method,omitempty"`
	// Columns are the key elements: column names, or deparsed expressions
	// such as "lower(email)".
	Columns []string `json:"columns,omitempty"`
	Include []string `json:"include,omitempty"`
	// Predicate is the deparsed WHERE clause of a partial index.
	Predicate string `json:"predicate,omitempty"`
}

// The constraint kinds of ConstraintDef.Kind.
const (
	ConstraintPrimaryKey = "primary_key"
	ConstraintUnique     = "unique"
	ConstraintForeignKey = "foreign_key"
	ConstraintCheck      = "check"
	ConstraintExclusion  = "exclusion"
	ConstraintOther      = "other"
)

// ConstraintDef describes an added, validated or dropped constraint, or a
// constraint of a created table or added column. Validate and drop name only
// the constraint, so Kind is empty there.
type ConstraintDef struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	// Columns are the constrained columns; for CHECK, the columns its
	// expression references (sorted).
	Columns     []string       `json:"columns,omitempty"`
	RefRelation *QualifiedName `json:"ref_relation,omitempty"`
	RefColumns  []string       `json:"ref_columns,omitempty"`
	NotValid    bool           `json:"not_valid,omitempty"`
	// Expr is the deparsed CHECK expression.
	Expr string `json:"expr,omitempty"`
	// UsingIndex is the index of ADD ... UNIQUE/PRIMARY KEY USING INDEX.
	UsingIndex string `json:"using_index,omitempty"`
}

// The generated forms of ColumnDef.Generated.
const (
	GeneratedStored            = "stored"
	GeneratedIdentityAlways    = "identity_always"
	GeneratedIdentityByDefault = "identity_by_default"
)

// ColumnDef describes the column of a column statement or created table.
type ColumnDef struct {
	Name string `json:"name"`
	// Type is the type as written, with pg_catalog dropped and internal names
	// used: "int4", "int8", "varchar(20)", "numeric(12,2)", "timestamp",
	// "timestamptz", "bool", "text[]", "serial". Empty when not given.
	Type string `json:"type,omitempty"`
	// NotNull is set by NOT NULL, PRIMARY KEY, an identity or a serial type.
	NotNull bool `json:"not_null,omitempty"`
	// HasDefault is set when new rows get a value without one being given:
	// DEFAULT, a serial type, an identity or a stored generated column.
	HasDefault bool `json:"has_default,omitempty"`
	// Default is the deparsed DEFAULT expression ("" for DROP DEFAULT).
	Default string `json:"default,omitempty"`
	// DefaultVolatile is set when the fill value is computed per row, so
	// ADD COLUMN rewrites the table: a default calling a function not known
	// to be stable or immutable, a serial type, an identity, or a stored
	// generated column. Unknown functions count as volatile.
	DefaultVolatile bool `json:"default_volatile,omitempty"`
	// Generated is "stored", "identity_always" or "identity_by_default".
	Generated string `json:"generated,omitempty"`
	// Using is the deparsed USING expression of alter_type.
	Using string `json:"using,omitempty"`
	// Constraints are the column's inline PRIMARY KEY, UNIQUE, REFERENCES and
	// CHECK constraints, with Columns set to this column.
	Constraints []ConstraintDef `json:"constraints,omitempty"`
}

// TableDef describes create_table.
type TableDef struct {
	Columns []ColumnDef `json:"columns,omitempty"`
	// Constraints are all constraints in declaration order: each column's
	// inline ones at the column's position, then table constraints.
	Constraints []ConstraintDef `json:"constraints,omitempty"`
	// PartitionBy is "range", "list" or "hash" for a partitioned table.
	PartitionBy string `json:"partition_by,omitempty"`
	// PartitionOf is the parent of CREATE TABLE ... PARTITION OF.
	PartitionOf *QualifiedName  `json:"partition_of,omitempty"`
	Inherits    []QualifiedName `json:"inherits,omitempty"`
	Unlogged    bool            `json:"unlogged,omitempty"`
	Temporary   bool            `json:"temporary,omitempty"`
}

// Setting describes SET and RESET.
type Setting struct {
	// Name is the lower-cased parameter ("" for RESET ALL).
	Name string `json:"name,omitempty"`
	// Value is the value as written, list items joined by ", ".
	Value string `json:"value,omitempty"`
	Local bool   `json:"local,omitempty"`
	// Reset is set for RESET, RESET ALL and SET ... TO DEFAULT.
	Reset bool `json:"reset,omitempty"`
}
