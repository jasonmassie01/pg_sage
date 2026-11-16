package workpack

// The section files of a pack. Each is canonical JSON (canon.go); every
// slice is in the order its comment states, and nothing in a section
// depends on when or by whom the pack was built.

// Header says what the pack is about. Days lists every UTC day the window
// touches, oldest first, with the tier it was read from.
type Header struct {
	Format              string       `json:"format"`
	Database            string       `json:"database"`
	Window              Window       `json:"window"`
	Timezone            string       `json:"timezone"`
	PGVersion           string       `json:"pg_version"`
	Platform            string       `json:"platform"`
	Capabilities        Capabilities `json:"capabilities"`
	HourlyRetentionDays int          `json:"hourly_retention_days"`
	Days                []Day        `json:"days"`
}

// Coverage is the evid-01b coverage report of the window: how much of the
// database's statement time the rollup speaks for, and its gaps. The
// history-wide fields (oldest hour and day, the rollup's size) are left
// out: they change when nothing in the window does.
type Coverage struct {
	ID                string   `json:"id"`
	Status            string   `json:"status"`
	CoveredExecShare  *float64 `json:"covered_exec_share"`
	EpochBreaks       int      `json:"epoch_breaks"`
	Hours             int      `json:"hours"`
	HoursMissing      int      `json:"hours_missing"`
	DeallocGrowth     *int64   `json:"dealloc_growth"`
	CountersOnlyHours int      `json:"epoch_source_counters_only_hours"`
}

// Workload is the window's statement families, by exec time (most first,
// then key), at most MaxFamilies, listing at most MaxQueryIDs queryids in
// all. Truncated is true when either bound cut something.
type Workload struct {
	Families      []Family `json:"families"`
	FamiliesTotal int      `json:"families_total"`
	QueryIDsTotal int      `json:"queryids_total"`
	Truncated     bool     `json:"truncated"`
	Order         string   `json:"order"`
}

// Family is one statement family: its totals over the window, its top
// queryids (at most TopQueryIDsPerFamily, by exec time then queryid) and
// its hour-of-week shape.
type Family struct {
	ID            string   `json:"id"`
	Key           string   `json:"key"`
	Command       string   `json:"command"`
	Relations     []string `json:"relations"`
	Calls         int64    `json:"calls"`
	ExecMs        float64  `json:"exec_ms"`
	Rows          int64    `json:"rows"`
	QueryIDsTotal int      `json:"queryids_total"`
	QueryIDs      []Query  `json:"queryids"`
	Shape         Shape    `json:"shape"`
}

// Query is one queryid's totals over the window. TextKnown is false when
// no queries snapshot of the window held its text.
type Query struct {
	ID        string  `json:"id"`
	QueryID   string  `json:"queryid"`
	Family    string  `json:"family"`
	Calls     int64   `json:"calls"`
	ExecMs    float64 `json:"exec_ms"`
	Rows      int64   `json:"rows"`
	TextKnown bool    `json:"text_known"`
}

// Shape is a family's hour-of-week profile over the hourly-tier part of
// the window (168 buckets, Monday 00:00 first, in the header's timezone):
// mean calls and exec ms per bucket hour, how many weeks each bucket was
// seen (hours with rollup coverage) and the buckets seen fewer than
// baseline.MinWeeks weeks (learning). Weeks is the most weeks any bucket
// was seen.
type Shape struct {
	Weeks       int       `json:"weeks"`
	CallsMean   []float64 `json:"calls_mean"`
	ExecMsMean  []float64 `json:"exec_ms_mean"`
	BucketWeeks []int     `json:"bucket_weeks"`
	Learning    []int     `json:"learning_buckets"`
}

// Growth is the window's trends: weekly calls and exec time per listed
// family (whole weeks from the window's start), and the daily database
// and table sizes (one value per header day; null when no snapshot of the
// day holds it, stated in missing_days).
type Growth struct {
	Weeks    []Window       `json:"weeks"`
	Workload []FamilyGrowth `json:"workload"`
	DBSize   Series         `json:"db_size"`
	Tables   []Series       `json:"tables"`
}

// FamilyGrowth is one family's weekly series, slope per week.
type FamilyGrowth struct {
	ID     string `json:"id"`
	Family string `json:"family"`
	Calls  Trend  `json:"calls"`
	ExecMs Trend  `json:"exec_ms"`
}

// Trend is a weekly series and its fit.
type Trend struct {
	Values []*float64 `json:"values"`
	Fit
}

// Series is a daily size series and its fit (slope in Unit per day).
type Series struct {
	ID          string     `json:"id"`
	Table       string     `json:"table,omitempty"`
	Unit        string     `json:"unit"`
	Values      []*float64 `json:"values"`
	MissingDays int        `json:"missing_days"`
	Fit         Fit        `json:"fit"`
}
