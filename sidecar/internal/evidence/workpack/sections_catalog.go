package workpack

// The catalog, settings, maintenance, plans, connections and context
// sections (evid-02b). Each is canonical JSON like the workload sections.
// A value pg_sage could not measure is the string "unknown" (or "absent"
// for something that does not exist), never zero and never an omitted
// section: a reader tells "not measured" from "measured as nothing".

// Section file names (evid-02b).
const (
	fileIndexes     = "indexes.json"
	fileSettings    = "settings.json"
	fileAutovacuum  = "autovacuum.json"
	fileSchema      = "schema.json"
	filePlans       = "plans.json"
	fileConnections = "connections.json"
	fileProvider    = "provider.json"
	fileStats       = "stats.json"
	fileContext     = "context.json"
)

// The words for a value that is not a measurement.
const (
	valueUnknown     = "unknown"
	valueAbsent      = "absent"
	valueUnreadable  = "unreadable"
	valueUnavailable = "unavailable"
)

// Bounds of the evid-02b sections.
const (
	MaxSchemaTables  = 2000
	MaxSchemaColumns = 20000
	MaxIndexes       = 5000
	MaxPlans         = 50
	MaxStatsColumns  = 5000
	MaxContextFacts  = 1000
)

// Indexes is every index of the database's user schemas (pg_sage's and
// the system schemas left out), by schema then name, at most MaxIndexes.
// ScansStatus says whether the window's scan counts are known: ok, reset
// (a relation-statistics reset inside the window) or unknown (no two
// snapshots, or an unrecorded statistics epoch).
type Indexes struct {
	Indexes     []IndexFact `json:"indexes"`
	Total       int         `json:"total"`
	Truncated   bool        `json:"truncated"`
	ScansStatus string      `json:"scans_status"`
}

// IndexFact is one index. Scans is the window's idx_scan growth or
// "unknown"; TableWrites is the table's inserts, updates and deletes over
// the window or "unknown". DuplicateOf names the indexes with the same
// table, method, keys, operator classes, collations, options, expressions
// and predicate; PrefixOf the indexes whose leading keys are this one's
// keys (both plain-column, same method and predicate).
type IndexFact struct {
	ID                string   `json:"id"`
	Schema            string   `json:"schema"`
	Table             string   `json:"table"`
	Index             string   `json:"index"`
	Definition        string   `json:"definition"`
	Method            string   `json:"method"`
	SizeBytes         int64    `json:"size_bytes"`
	Unique            bool     `json:"unique"`
	Primary           bool     `json:"primary"`
	ConstraintBacking bool     `json:"constraint_backing"`
	Valid             bool     `json:"valid"`
	Partial           bool     `json:"partial"`
	Scans             any      `json:"scans"`
	TableWrites       any      `json:"table_writes"`
	DuplicateOf       []string `json:"duplicate_of"`
	PrefixOf          []string `json:"prefix_of"`
}

// Writes is a table's row changes over the window.
type Writes struct {
	Inserts int64 `json:"inserts"`
	Updates int64 `json:"updates"`
	Deletes int64 `json:"deletes"`
}

// Settings is the server settings of the window's last config snapshot,
// by name, each with its changes inside the window, the workload measures
// some are judged by, and the hardware they run on.
type Settings struct {
	Status   string        `json:"status"`
	Settings []SettingFact `json:"settings"`
	Measures Measures      `json:"measures"`
	Hardware Hardware      `json:"hardware"`
}

// SettingFact is one setting. Changes are the days whose snapshot holds a
// different value than the day before, oldest first.
type SettingFact struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Value   string          `json:"value"`
	Unit    string          `json:"unit"`
	Source  string          `json:"source"`
	Changes []SettingChange `json:"changes"`
	Measure string          `json:"measure,omitempty"`
}

// SettingChange is one day's change of a setting.
type SettingChange struct {
	Day  string `json:"day"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Measures are the workload numbers settings are judged by, each a number
// or "unknown": temp bytes per hour (work_mem), requested and timed
// checkpoints and WAL bytes per hour (max_wal_size), and the peak client
// backends against max_connections less the reserved slots.
type Measures struct {
	TempBytesPerHour     any `json:"temp_bytes_per_hour"`
	CheckpointsRequested any `json:"checkpoints_requested"`
	CheckpointsTimed     any `json:"checkpoints_timed"`
	WALBytesPerHour      any `json:"wal_bytes_per_hour"`
	PeakClientBackends   any `json:"peak_client_backends"`
	UsableConnections    any `json:"usable_connections"`
}

// Hardware is what the database runs on, from the cloud telemetry of its
// provider resource, or "unknown".
type Hardware struct {
	VCPUs       any `json:"vcpus"`
	MemoryBytes any `json:"memory_bytes"`
}

// Autovacuum is each table's vacuum and analyze history over the window,
// by schema then table, at most MaxSchemaTables. DeadTuples has one value
// per header day (null when no snapshot of the day holds the table).
type Autovacuum struct {
	Tables    []AutovacuumFact `json:"tables"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
	Days      []string         `json:"days"`
}

// AutovacuumFact is one table's maintenance history. LiveTuples is the
// window's last tables snapshot's n_live_tup, or "unknown" (review-02b).
// Reloptions are the storage parameters that parse as name=value;
// ReloptionsText the ones that do not, kept as written.
type AutovacuumFact struct {
	ID              string            `json:"id"`
	Table           string            `json:"table"`
	LiveTuples      any               `json:"live_tuples"`
	Autovacuums     any               `json:"autovacuums"`
	Autoanalyzes    any               `json:"autoanalyzes"`
	LastAutovacuum  any               `json:"last_autovacuum"`
	LastAutoanalyze any               `json:"last_autoanalyze"`
	DeadTuples      []*int64          `json:"dead_tuples"`
	Reloptions      map[string]string `json:"reloptions"`
	ReloptionsText  []string          `json:"reloptions_text"`
}

// Schema is the database's tables (partitions folded into their root),
// with columns, constraints and index definitions, by schema then name;
// at most MaxSchemaTables tables and MaxSchemaColumns columns.
type Schema struct {
	Tables       []TableFact `json:"tables"`
	TablesTotal  int         `json:"tables_total"`
	ColumnsTotal int         `json:"columns_total"`
	Truncated    bool        `json:"truncated"`
}

// TableFact is one table or partition root.
type TableFact struct {
	ID             string       `json:"id"`
	Table          string       `json:"table"`
	Kind           string       `json:"kind"`
	SizeBytes      int64        `json:"size_bytes"`
	PartitionKey   string       `json:"partition_key,omitempty"`
	PartitionCount int          `json:"partition_count"`
	Columns        []ColumnFact `json:"columns"`
	Constraints    []string     `json:"constraints"`
	Indexes        []string     `json:"indexes"`
}

// ColumnFact is one column.
type ColumnFact struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null"`
}

// Plans is the latest captured plan of each of the window's top queryids
// (by exec time, at most MaxPlans), as shape only.
type Plans struct {
	Status string     `json:"status"`
	Plans  []PlanFact `json:"plans"`
}

// PlanFact is one queryid's latest plan. Plan is "ok", "absent" (none
// captured) or "unreadable" (not EXPLAIN JSON). Conditions is the full
// plan with its literals redacted, only when the build asked for it.
type PlanFact struct {
	ID          string `json:"id"`
	QueryID     string `json:"queryid"`
	Plan        string `json:"plan"`
	Source      string `json:"source,omitempty"`
	CapturedAt  string `json:"captured_at,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Shape       any    `json:"shape,omitempty"`
	Conditions  any    `json:"conditions,omitempty"`
}

// Connections is the window's client sessions per hour of week (168
// buckets, Monday 00:00 first, in the header's timezone): the mean count
// per snapshot by state and by connection-age bucket.
type Connections struct {
	Hours []ConnectionHour `json:"hours"`
}

// ConnectionHour is one hour-of-week bucket. States and Ages are maps of
// mean sessions, or "unknown" (no snapshot, or a snapshot written before
// ages were collected).
type ConnectionHour struct {
	ID        string `json:"id"`
	Hour      int    `json:"hour"`
	Snapshots int    `json:"snapshots"`
	States    any    `json:"states"`
	Ages      any    `json:"ages"`
}

// Provider is what the cloud telemetry knows of the database's instance:
// a status fact and, when known, one fact each.
type Provider struct {
	Status     string         `json:"status"`
	ErrorClass string         `json:"error_class,omitempty"`
	Facts      []ProviderFact `json:"facts"`
}

// ProviderFact is one provider fact.
type ProviderFact struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// Stats is pg_stats for the columns of the tables the listed families
// reference, by table then column, at most MaxStatsColumns.
type Stats struct {
	Tables        []StatsTable  `json:"tables"`
	Columns       []ColumnStats `json:"columns"`
	ValueSamples  bool          `json:"value_samples"`
	Truncated     bool          `json:"truncated"`
	ColumnsListed int           `json:"columns_listed"`
}

// StatsTable says whether a referenced table's statistics were readable:
// ok, or unknown (no table, no privilege, or never analyzed).
type StatsTable struct {
	ID     string `json:"id"`
	Table  string `json:"table"`
	Status string `json:"status"`
}

// ColumnStats is one column's planner statistics. MostCommonVals and
// HistogramBounds are present only when the build asked for value
// samples.
type ColumnStats struct {
	ID              string   `json:"id"`
	Table           string   `json:"table"`
	Column          string   `json:"column"`
	NullFrac        float64  `json:"null_frac"`
	NDistinct       float64  `json:"n_distinct"`
	Correlation     *float64 `json:"correlation"`
	AvgWidth        int      `json:"avg_width"`
	MostCommonVals  *string  `json:"most_common_vals,omitempty"`
	HistogramBounds *string  `json:"histogram_bounds,omitempty"`
}

// Context is what the owner said: the confirmed owner brief (or
// "absent") and the confirmed binding facts, with who confirmed them and
// when.
type Context struct {
	Brief any           `json:"brief"`
	Facts []ContextFact `json:"facts"`
}

// ContextFact is one confirmed fact (the brief included, as Brief).
type ContextFact struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	SubjectKind string            `json:"subject_kind"`
	Subject     string            `json:"subject"`
	Value       map[string]string `json:"value"`
	ConfirmedBy string            `json:"confirmed_by"`
	ConfirmedAt string            `json:"confirmed_at"`
	ExpiresAt   string            `json:"expires_at,omitempty"`
}
