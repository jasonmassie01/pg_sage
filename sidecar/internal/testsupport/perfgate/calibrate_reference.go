package perfgate

// referenceRun is one large-scale perfgate.yml run on a GitHub runner,
// dispatched to set the reference runner's workload times: each the best
// of calibrationRuns timings, as the gate's calibration logs them.
type referenceRun struct {
	id       int64  // GitHub Actions run ID
	date     string // UTC, YYYY-MM-DD
	cpuModel string // /proc/cpuinfo model name
	cpus     int
	cpuMs    float64
	dbMs     float64
}

// referenceRuns are the runs ReferenceCPUMs and ReferenceDBMs are the
// medians of: twelve dispatches of perfgate.yml at scale=large on
// 2026-10-10, each on its own ubuntu-latest runner with the gate's PG17
// container from GHCR. Nine ran on AMD EPYC 7763, two on Intel Xeon
// Platinum 8573C and one on AMD EPYC 9V74, whose CPU and SQL speeds differ
// (x0.86 and x1.04 of the reference): the factors are separate for that.
var referenceRuns = []referenceRun{
	{38015820650, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 62.8, 129.8},
	{38015822540, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 63.4, 133.5},
	{38015824321, "2026-10-10", "INTEL(R) XEON(R) PLATINUM 8573C", 4, 46.6, 97.1},
	{38015826481, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 63.5, 130.3},
	{38015829367, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 63.2, 129.7},
	{38015831421, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 63.3, 129.9},
	{38015833165, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 64.0, 129.2},
	{38015835008, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 63.9, 125.6},
	{38015838355, "2026-10-10", "INTEL(R) XEON(R) PLATINUM 8573C", 4, 46.1, 92.3},
	{38015841387, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 62.6, 129.3},
	{38015843103, "2026-10-10", "AMD EPYC 9V74 80-Core Processor", 4, 54.3, 134.3},
	{38015845038, "2026-10-10", "AMD EPYC 7763 64-Core Processor", 4, 62.6, 130.3},
}
