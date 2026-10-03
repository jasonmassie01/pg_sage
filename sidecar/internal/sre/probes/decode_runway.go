package probes

import (
	"fmt"
	"math"
	"time"
)

// M6 runway decoders. Unknown numbers are NaN, never zero; an
// unavailable result is an *UnavailableError.

// Runway series kinds, as the runway monitor samples them into
// sage.runway_samples and runway_trends reads them back.
const (
	RunwayXID           = "xid"
	RunwayMXID          = "mxid"
	RunwayWALPosition   = "wal_position"
	RunwayWALSlot       = "wal_slot"
	RunwayDatabaseBytes = "database_bytes"
	RunwayDiskUsed      = "disk_used"
	RunwaySequence      = "sequence"
)

// SubjectCluster names a cluster-wide runway series.
const SubjectCluster = "cluster"

// Kinds of xmin-horizon holders.
const (
	HolderSession      = "session"
	HolderStandby      = "standby"
	HolderPreparedXact = "prepared_xact"
	HolderSlot         = "slot"
	HolderSlotCatalog  = "slot_catalog"
)

// The limit a sequence runs into first.
const (
	LimitSequenceType = "sequence_type"
	LimitColumnType   = "column_type"
	LimitExplicitMax  = "explicit_maxvalue"
)

// XIDRunway is the cluster's transaction-ID and multixact runway with
// the autovacuum capacity that keeps it.
type XIDRunway struct {
	NextXID          float64
	MXIDCounter      float64
	ClusterXIDAge    float64
	ClusterMXIDAge   float64
	DatabaseXIDAge   float64
	OldestDatabase   string
	FreezeMaxAge     float64
	MXIDFreezeMaxAge float64
	AutovacuumOn     bool
	MaxWorkers       float64
	Workers          float64
}

// WorkersSaturated reports every autovacuum worker busy; an unknown
// maximum is never saturated.
func (x XIDRunway) WorkersSaturated() bool {
	return Known(x.MaxWorkers) && x.MaxWorkers > 0 && Known(x.Workers) &&
		x.Workers >= x.MaxWorkers
}

// WraparoundTable is one table's freeze horizon against its effective
// maximum (its reloption when lower than the setting).
type WraparoundTable struct {
	Relation          string
	XIDAge            float64
	MXIDAge           float64
	FreezeMaxAge      float64
	MXIDFreezeMaxAge  float64
	AutovacuumEnabled bool
	DeadTuples        float64
	LastVacuumAgeS    float64
	AutovacuumCount   float64
	VacuumRunning     bool
}

// Fraction is the larger share of the XID or multixact freeze maximum
// the table's age has used (1 is the maximum); NaN when neither is known.
func (t WraparoundTable) Fraction() float64 {
	f := math.NaN()
	for _, p := range [][2]float64{{t.XIDAge, t.FreezeMaxAge},
		{t.MXIDAge, t.MXIDFreezeMaxAge}} {
		if !Known(p[0]) || !(p[1] > 0) {
			continue
		}
		if v := p[0] / p[1]; math.IsNaN(f) || v > f {
			f = v
		}
	}
	return f
}

// XminHolder is one holder of the xmin horizon: a session (by pid and
// backend start), a standby's feedback, a prepared transaction (by gid
// hash) or a replication slot (by name).
type XminHolder struct {
	Kind         string
	PID          int64
	BackendStart time.Time
	Name         string
	State        string
	XminAge      float64
	XactAgeS     float64
	Application  string
	Active       bool
}

// WALRunway is the WAL position, the WAL size settings in bytes, the
// cluster and role that answered, and the databases' size. The size is
// not part of wal_runway: it is the cluster_database_size measurement
// (ClusterSizeOf), NaN until a caller fills it in.
type WALRunway struct {
	PositionBytes       float64
	MaxWALSize          float64
	WALKeepSize         float64
	MaxSlotWALKeepSize  float64
	SegmentSize         float64
	DatabaseBytes       float64
	UnreadableDatabases float64
	InRecovery          bool
	SystemID            string
	StartedAt           time.Time
	RoleName            string
}

// SlotKeepBounded reports a max_slot_wal_keep_size that bounds slots.
func (w WALRunway) SlotKeepBounded() bool {
	return Known(w.MaxSlotWALKeepSize) && w.MaxSlotWALKeepSize >= 0
}

// WALDirectory is pg_wal's size and the segments waiting for the archiver.
type WALDirectory struct {
	Bytes      float64
	Files      float64
	ReadyFiles float64
}

// SequenceRunway is one ascending sequence against its effective limit.
type SequenceRunway struct {
	Sequence     string
	DataType     string
	Increment    float64
	Cycle        bool
	LastValue    float64
	MinValue     float64
	MaxValue     float64
	TypeMax      float64
	OwnerColumn  string
	OwnerType    string
	OwnerTypeMax float64
	Limit        float64
	Fraction     float64
}

// BindingLimit names the limit the sequence reaches first: an owning
// column narrower than the sequence, an explicit MAXVALUE below the
// sequence's type, or the sequence's own type.
func (s SequenceRunway) BindingLimit() string {
	switch {
	case Known(s.OwnerTypeMax) && s.OwnerTypeMax < s.MaxValue:
		return LimitColumnType
	case Known(s.TypeMax) && s.MaxValue < s.TypeMax:
		return LimitExplicitMax
	default:
		return LimitSequenceType
	}
}

// RunwayTrend is one sampled series regressed over a window.
type RunwayTrend struct {
	Kind      string
	Subject   string
	Samples   int64
	FirstAt   time.Time
	LastAt    time.Time
	LastValue float64
	Limit     float64
	RatePerS  float64
	R2        float64
}

// SpanS is the seconds between the first and last sample.
func (t RunwayTrend) SpanS() float64 {
	if t.FirstAt.IsZero() || t.LastAt.IsZero() {
		return 0
	}
	return t.LastAt.Sub(t.FirstAt).Seconds()
}

// SecondsToLimit projects when the series reaches its limit at its
// measured rate: 0 at or past the limit, +Inf when flat or falling, NaN
// when the value, limit or rate is unknown.
func (t RunwayTrend) SecondsToLimit() float64 {
	if !Known(t.LastValue) || !Known(t.Limit) {
		return math.NaN()
	}
	if t.LastValue >= t.Limit {
		return 0
	}
	if !Known(t.RatePerS) {
		return math.NaN()
	}
	if t.RatePerS <= 0 {
		return math.Inf(1)
	}
	return (t.Limit - t.LastValue) / t.RatePerS
}

// FindTrend returns the trend of one series.
func FindTrend(ts []RunwayTrend, kind, subject string) (RunwayTrend, bool) {
	for _, t := range ts {
		if t.Kind == kind && t.Subject == subject {
			return t, true
		}
	}
	return RunwayTrend{}, false
}

// XIDRunwayOf decodes an xid_runway result.
func XIDRunwayOf(res Result) (XIDRunway, error) {
	r, err := singleRow(res, XIDRunwayProbe)
	if err != nil {
		return XIDRunway{}, err
	}
	return XIDRunway{NextXID: floatField(r, "next_xid"),
		MXIDCounter:      floatField(r, "mxid_counter"),
		ClusterXIDAge:    floatField(r, "cluster_xid_age"),
		ClusterMXIDAge:   floatField(r, "cluster_mxid_age"),
		DatabaseXIDAge:   floatField(r, "database_xid_age"),
		OldestDatabase:   strField(r, "oldest_database"),
		FreezeMaxAge:     floatField(r, "freeze_max_age"),
		MXIDFreezeMaxAge: floatField(r, "multixact_freeze_max_age"),
		AutovacuumOn:     boolField(r, "autovacuum_on"),
		MaxWorkers:       floatField(r, "autovacuum_max_workers"),
		Workers:          floatField(r, "autovacuum_workers")}, nil
}

// WraparoundTables decodes a wraparound_tables result.
func WraparoundTables(res Result) ([]WraparoundTable, error) {
	rows, err := rowsFor(res, WraparoundTablesProbe)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]WraparoundTable, 0, len(rows))
	for i, r := range rows {
		rel := strField(r, "relation")
		if rel == "" {
			return nil, fmt.Errorf("wraparound_tables row %d: relation is missing", i+1)
		}
		out = append(out, WraparoundTable{Relation: rel,
			XIDAge: floatField(r, "xid_age"), MXIDAge: floatField(r, "mxid_age"),
			FreezeMaxAge:      floatField(r, "freeze_max_age"),
			MXIDFreezeMaxAge:  floatField(r, "mxid_freeze_max_age"),
			AutovacuumEnabled: boolField(r, "autovacuum_enabled"),
			DeadTuples:        floatField(r, "n_dead_tup"),
			LastVacuumAgeS:    floatField(r, "last_vacuum_age_s"),
			AutovacuumCount:   floatField(r, "autovacuum_count"),
			VacuumRunning:     boolField(r, "vacuum_running")})
	}
	return out, nil
}

// XminHolders decodes an xmin_horizon result, oldest first.
func XminHolders(res Result) ([]XminHolder, error) {
	rows, err := rowsFor(res, XminHorizon)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]XminHolder, 0, len(rows))
	for i, r := range rows {
		kind := strField(r, "holder_kind")
		if kind == "" {
			return nil, fmt.Errorf("xmin_horizon row %d: holder_kind is missing", i+1)
		}
		pid, _ := intField(r, "pid")
		out = append(out, XminHolder{Kind: kind, PID: pid,
			BackendStart: timeField(r, "backend_start"), Name: strField(r, "holder_name"),
			State: strField(r, "state"), XminAge: floatField(r, "xmin_age"),
			XactAgeS: floatField(r, "xact_age_s"), Application: strField(r, "application_name"),
			Active: boolField(r, "active")})
	}
	return out, nil
}

// AutovacuumCancellationCount decodes autovacuum_cancellations: the
// logged-cancellation incidents in the window.
func AutovacuumCancellationCount(res Result) (int64, error) {
	r, err := singleRow(res, AutovacuumCancellations)
	if err != nil {
		return 0, err
	}
	n, err := intField(r, "cancel_incidents")
	if err != nil {
		return 0, fmt.Errorf("autovacuum_cancellations: %w", err)
	}
	return n, nil
}

// WALRunwayOf decodes a wal_runway result.
func WALRunwayOf(res Result) (WALRunway, error) {
	r, err := singleRow(res, WALRunwayProbe)
	if err != nil {
		return WALRunway{}, err
	}
	return WALRunway{PositionBytes: floatField(r, "wal_position_bytes"),
		MaxWALSize:         floatField(r, "max_wal_size_bytes"),
		WALKeepSize:        floatField(r, "wal_keep_size_bytes"),
		MaxSlotWALKeepSize: floatField(r, "max_slot_wal_keep_size_bytes"),
		SegmentSize:        floatField(r, "wal_segment_size_bytes"),
		DatabaseBytes:      math.NaN(), UnreadableDatabases: math.NaN(),
		InRecovery: boolField(r, "in_recovery"), SystemID: strField(r, "system_identifier"),
		StartedAt: timeField(r, "server_started_at"), RoleName: strField(r, "role_name")}, nil
}

// ClusterSizeOf decodes a cluster_database_size result: the bytes of
// every database this role may connect to and how many it may not
// (unknown numbers NaN).
func ClusterSizeOf(res Result) (bytes, unreadable float64, err error) {
	r, err := singleRow(res, ClusterDatabaseSizeProbe)
	if err != nil {
		return math.NaN(), math.NaN(), err
	}
	return floatField(r, "database_bytes"), floatField(r, "databases_unreadable"), nil
}

// WALDirectoryOf decodes a wal_directory result.
func WALDirectoryOf(res Result) (WALDirectory, error) {
	r, err := singleRow(res, WALDirectoryProbe)
	if err != nil {
		return WALDirectory{}, err
	}
	return WALDirectory{Bytes: floatField(r, "wal_dir_bytes"),
			Files: floatField(r, "wal_files"), ReadyFiles: floatField(r, "archive_ready_files")},
		nil
}

// Sequences decodes a sequence_runway result, nearest its limit first
// (a coverage_only row lists no sequence).
func Sequences(res Result) ([]SequenceRunway, error) {
	rows, err := rowsFor(res, SequenceRunwayProbe)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]SequenceRunway, 0, len(rows))
	for i, r := range rows {
		if coverageOnly(r) {
			continue
		}
		name := strField(r, "sequence")
		if name == "" {
			return nil, fmt.Errorf("sequence_runway row %d: sequence is missing", i+1)
		}
		out = append(out, SequenceRunway{Sequence: name,
			DataType: strField(r, "data_type"), Increment: floatField(r, "increment_by"),
			Cycle: boolField(r, "cycle"), LastValue: floatField(r, "last_value"),
			MinValue: floatField(r, "min_value"), MaxValue: floatField(r, "max_value"),
			TypeMax: floatField(r, "type_max"), OwnerColumn: strField(r, "owner_column"),
			OwnerType: strField(r, "owner_type"), OwnerTypeMax: floatField(r, "owner_type_max"),
			Limit: floatField(r, "effective_limit"), Fraction: floatField(r, "fraction_used")})
	}
	return out, nil
}

// RunwayTrends decodes a runway_trends result.
func RunwayTrends(res Result) ([]RunwayTrend, error) {
	rows, err := rowsFor(res, RunwayTrendsProbe)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]RunwayTrend, 0, len(rows))
	for i, r := range rows {
		kind, subject := strField(r, "kind"), strField(r, "subject")
		n, nerr := intField(r, "samples")
		if kind == "" || subject == "" || nerr != nil {
			return nil, fmt.Errorf("runway_trends row %d: kind, subject and samples "+
				"are required", i+1)
		}
		out = append(out, RunwayTrend{Kind: kind, Subject: subject, Samples: n,
			FirstAt: timeField(r, "first_at"), LastAt: timeField(r, "last_at"),
			LastValue: floatField(r, "last_value"), Limit: floatField(r, "last_limit"),
			RatePerS: floatField(r, "rate_per_s"), R2: floatField(r, "r2")})
	}
	return out, nil
}
