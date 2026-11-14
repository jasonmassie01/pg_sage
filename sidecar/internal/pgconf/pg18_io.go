package pgconf

// AdviceOnlyDocs documents settings the advisor reads and explains but
// never proposes as SQL (market-15). They are kept apart from Docs, which
// is the advisor's executable list: AdvisorGUC and ExecutableGUC stay false
// for every name here, so documenting them adds no write path.
var AdviceOnlyDocs = map[string]GUCDoc{
	"io_method": {
		Description: "How PostgreSQL issues reads: synchronously or asynchronously.",
		Guidance: "Keep the default (worker) unless measurements favour io_uring; " +
			"sync switches asynchronous I/O off.",
		VersionNotes: []VersionNote{{MinMajor: 18, Text: "PostgreSQL 18 added io_method: " +
			"sync (no asynchronous I/O), worker (the default; reads go through the " +
			"io_workers processes) or io_uring (only on a Linux build with liburing). " +
			"A change needs a restart."}},
	},
	"io_workers": {
		Description: "Number of I/O worker processes that perform asynchronous reads.",
		Guidance:    "Raise it when the I/O workers are saturated on a read-heavy load.",
		Unit:        "count", SafeMin: 1, SafeMax: 32,
		VersionNotes: []VersionNote{{MinMajor: 18, Text: "PostgreSQL 18: used only with " +
			"io_method = worker; the default is 3, and a reload applies a change."}},
	},
	"io_combine_limit": {
		Description: "Largest read that combines adjacent blocks into one I/O.",
		Guidance:    "The default suits most storage; a larger value helps sequential reads.",
		Unit:        "bytes", BaseUnit: "8kB",
		VersionNotes: []VersionNote{
			{MinMajor: 17, Text: "Added in PostgreSQL 17; the default is 128kB."},
			{MinMajor: 18, Text: "PostgreSQL 18 caps it at io_max_combine_limit, " +
				"which needs a restart to change."},
		},
	},
	"maintenance_io_concurrency": {
		Description: "Concurrent I/O operations maintenance work (VACUUM, ANALYZE, " +
			"index builds) may issue.",
		Guidance: "Like effective_io_concurrency: higher for SSD and NVMe storage.",
		Unit:     "count", SafeMin: 0, SafeMax: 1000,
		VersionNotes: []VersionNote{{MinMajor: 18, Text: "PostgreSQL 18 raised the " +
			"default to 16 (from 10); a value of 10 or less set by a configuration file " +
			"is likely carried over from an older server and limits maintenance " +
			"read-ahead."}},
	},
}
