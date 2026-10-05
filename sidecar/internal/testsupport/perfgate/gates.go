package perfgate

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Gate names one budget. The constants are in report order.
type Gate string

const (
	GateSeqScan       Gate = "A seq scan of a large sage table"
	GateStatementMean Gate = "B statement mean time"
	GateCycleDBTime   Gate = "B pg_sage DB time per cycle"
	GateRowsWritten   Gate = "C rows written per cycle"
	GateCatalogMax    Gate = "D catalog statement max time"
	GateTimeout       Gate = "D statement cut off by a timeout"
	GateEndpoint      Gate = "E API list endpoint"
	GateHotUpdates    Gate = "F HOT share of updates"
	GateSidecarCPU    Gate = "G sidecar CPU per cycle"
)

var gateOrder = map[Gate]int{
	GateSeqScan: 0, GateStatementMean: 1, GateCycleDBTime: 2, GateRowsWritten: 3,
	GateCatalogMax: 4, GateTimeout: 5, GateEndpoint: 6, GateHotUpdates: 7,
	GateSidecarCPU: 8,
}

// TableDelta is one sage table's counters over a phase.
type TableDelta struct {
	Name        string // schema-qualified
	LiveRows    int64  // at the end of the phase
	SeqScans    int64
	SeqTupRead  int64
	IdxScans    int64
	RowsWritten int64 // inserted + updated + deleted
	Updates     int64
	HotUpdates  int64 // updates that wrote no index entry (heap-only tuples)
	Bytes       int64 // heap, TOAST and indexes at the end of the phase
	BytesGrowth int64 // Bytes minus the size at the start of the phase
	Relations   int   // the table itself, or its partitions
	// ScannedRelationRows is the live rows of the largest relation (the
	// table or one partition) scanned sequentially in the phase.
	ScannedRelationRows int64
}

// Statement is one pg_stat_statements entry of pg_sage's.
type Statement struct {
	QueryID int64
	Query   string
	Calls   int64
	TotalMs float64
	MeanMs  float64
	MaxMs   float64
	Rows    int64
}

// PlanScan is a sequential scan in a statement's generic plan.
type PlanScan struct {
	Schema   string
	Relation string
}

// PlanResult is a statement's generic plan summary; Err is set when it
// could not be planned.
type PlanResult struct {
	Statement Statement
	SeqScans  []PlanScan
	Err       string
}

// Endpoint is an API endpoint's measurement: Duration is the charged
// time (the median of Samples when there are several calls).
type Endpoint struct {
	Path     string
	Status   int
	Duration time.Duration
	Samples  []time.Duration
}

// Phase is one measured stretch of the run. Steady phases are charged
// the per-cycle and per-statement budgets; every phase is charged the
// seq-scan, catalog and timeout budgets; the HOT budget is steady-only.
type Phase struct {
	Name       string
	Steady     bool
	Window     time.Duration
	Cycles     int
	Tables     []TableDelta
	Statements []Statement
	Plans      []PlanResult
	Timeouts   []string
	Endpoints  []Endpoint
	// ProcessCPU is the sidecar process's CPU time over the phase, API
	// calls excluded; CPUKnown is false when it was not read.
	ProcessCPU time.Duration
	CPUKnown   bool
}

// Offender is one budget breach.
type Offender struct {
	Gate     Gate
	Phase    string
	Subject  string
	Measured float64
	Budget   float64
	Unit     string
	Detail   string
}

// ratio is how far over budget an offender is (a floor gate is breached by
// being under its budget).
func (o Offender) ratio() float64 {
	if o.Gate == GateHotUpdates {
		return o.Budget / math.Max(o.Measured, 0.1)
	}
	return o.Measured / o.Budget
}

// Evaluate charges every phase against the budgets and returns the
// offenders ranked by gate, then by how far over budget they are.
func Evaluate(phases []Phase, b Budgets) ([]Offender, error) {
	if len(phases) == 0 {
		return nil, errors.New("perfgate: no phases to evaluate")
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	var out []Offender
	for _, p := range phases {
		if p.Steady && p.Cycles <= 0 {
			return nil, fmt.Errorf("perfgate: steady phase %q has %d cycles", p.Name, p.Cycles)
		}
		out = append(out, seqScanOffenders(p, b)...)
		if p.Steady {
			out = append(out, timeOffenders(p, b)...)
			out = append(out, writeOffenders(p, b)...)
			out = append(out, hotOffenders(p, b)...)
			out = append(out, cpuOffenders(p, b)...)
		}
		out = append(out, catalogOffenders(p, b)...)
		out = append(out, endpointOffenders(p, b)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if gateOrder[out[i].Gate] != gateOrder[out[j].Gate] {
			return gateOrder[out[i].Gate] < gateOrder[out[j].Gate]
		}
		return out[i].ratio() > out[j].ratio()
	})
	return out, nil
}

func seqScanOffenders(p Phase, b Budgets) []Offender {
	var out []Offender
	for _, t := range p.Tables {
		if t.SeqScans <= 0 || scannedRows(t) <= b.SeqScanMinRows {
			continue
		}
		out = append(out, Offender{Gate: GateSeqScan, Phase: p.Name, Subject: t.Name,
			Measured: float64(t.SeqTupRead), Budget: float64(b.SeqScanMinRows),
			Unit: "rows read sequentially",
			Detail: fmt.Sprintf("%d seq scans of %d live rows; %s", t.SeqScans,
				t.LiveRows, suspects(p, t.Name))})
	}
	return out
}

// scannedRows is the size of the largest relation scanned sequentially: a
// table built by hand (no relation count) is judged by its live rows.
func scannedRows(t TableDelta) int64 {
	if t.Relations == 0 {
		return t.LiveRows
	}
	return t.ScannedRelationRows
}

// suspects names the statements whose generic plan scans table
// sequentially, costliest first.
func suspects(p Phase, table string) string {
	var hits []Statement
	for _, plan := range p.Plans {
		for _, s := range plan.SeqScans {
			if scans(s, table) {
				hits = append(hits, plan.Statement)
				break
			}
		}
	}
	if len(hits) == 0 {
		return "no captured statement plans a seq scan of it " +
			"(a foreign-key action, a custom plan or a non-top-level statement)"
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].TotalMs > hits[j].TotalMs })
	parts := make([]string, 0, len(hits))
	for _, s := range hits {
		parts = append(parts, fmt.Sprintf("queryid %d (%.0f ms total): %s",
			s.QueryID, s.TotalMs, shortQuery(s.Query)))
	}
	return "suspects: " + strings.Join(parts, " | ")
}

func timeOffenders(p Phase, b Budgets) []Offender {
	var out []Offender
	var total float64
	for _, s := range p.Statements {
		total += s.TotalMs
		if s.MeanMs > b.StatementMeanMs {
			out = append(out, Offender{Gate: GateStatementMean, Phase: p.Name,
				Subject: statementSubject(s), Measured: s.MeanMs, Budget: b.StatementMeanMs,
				Unit:   "ms mean",
				Detail: fmt.Sprintf("%d calls, %.0f ms total, max %.0f ms", s.Calls, s.TotalMs, s.MaxMs)})
		}
	}
	perCycle := total / float64(p.Cycles)
	if perCycle > b.CycleDBTimeMs {
		out = append(out, Offender{Gate: GateCycleDBTime, Phase: p.Name,
			Subject: "all pg_sage statements", Measured: perCycle, Budget: b.CycleDBTimeMs,
			Unit:   "ms per cycle",
			Detail: fmt.Sprintf("%.0f ms over %d cycles", total, p.Cycles)})
	}
	return out
}

func writeOffenders(p Phase, b Budgets) []Offender {
	var out []Offender
	for _, t := range p.Tables {
		perCycle := float64(t.RowsWritten) / float64(p.Cycles)
		if perCycle > float64(b.RowsWrittenPerCycle) {
			out = append(out, Offender{Gate: GateRowsWritten, Phase: p.Name, Subject: t.Name,
				Measured: perCycle, Budget: float64(b.RowsWrittenPerCycle),
				Unit:   "rows per cycle",
				Detail: fmt.Sprintf("%d rows written over %d cycles", t.RowsWritten, p.Cycles)})
		}
	}
	return out
}

// hotOffenders: a sage table updated at least HotMinUpdates times in the
// phase must write at least HotUpdateMinPct percent of its updates as
// heap-only tuples. A non-HOT update writes a new entry in every index and
// leaves dead index entries for vacuum; an updated column that is indexed
// (or a full page) causes it.
func hotOffenders(p Phase, b Budgets) []Offender {
	var out []Offender
	for _, t := range p.Tables {
		if t.Updates < b.HotMinUpdates {
			continue
		}
		pct := float64(t.HotUpdates) * 100 / float64(t.Updates)
		if pct >= b.HotUpdateMinPct {
			continue
		}
		out = append(out, Offender{Gate: GateHotUpdates, Phase: p.Name, Subject: t.Name,
			Measured: pct, Budget: b.HotUpdateMinPct, Unit: "% HOT",
			Detail: fmt.Sprintf("%d of %d updates HOT", t.HotUpdates, t.Updates)})
	}
	return out
}

func catalogOffenders(p Phase, b Budgets) []Offender {
	var out []Offender
	for _, s := range p.Statements {
		if s.MaxMs > b.CatalogStatementMaxMs && IsCatalogQuery(s.Query) {
			out = append(out, Offender{Gate: GateCatalogMax, Phase: p.Name,
				Subject: statementSubject(s), Measured: s.MaxMs, Budget: b.CatalogStatementMaxMs,
				Unit:   "ms max",
				Detail: fmt.Sprintf("%d calls, mean %.0f ms", s.Calls, s.MeanMs)})
		}
	}
	seen := map[string]int{}
	var order []string
	for _, line := range p.Timeouts {
		if seen[line] == 0 {
			order = append(order, line)
		}
		seen[line]++
	}
	for _, line := range order {
		out = append(out, Offender{Gate: GateTimeout, Phase: p.Name, Subject: line,
			Measured: float64(seen[line]), Budget: 1, Unit: "occurrences",
			Detail: "pg_stat_statements does not record cancelled statements"})
	}
	return out
}

func endpointOffenders(p Phase, b Budgets) []Offender {
	var out []Offender
	for _, e := range p.Endpoints {
		ms := float64(e.Duration) / float64(time.Millisecond)
		if e.Status == 200 && ms <= b.EndpointMaxMs {
			continue
		}
		out = append(out, Offender{Gate: GateEndpoint, Phase: p.Name, Subject: e.Path,
			Measured: ms, Budget: b.EndpointMaxMs, Unit: "ms",
			Detail: fmt.Sprintf("HTTP %d, median of %d calls, max %.0f ms", e.Status,
				e.Calls(), float64(e.Max())/float64(time.Millisecond))})
	}
	return out
}

// cpuOffenders charges the steady phase's sidecar CPU per cycle.
func cpuOffenders(p Phase, b Budgets) []Offender {
	if !p.CPUKnown || p.Cycles <= 0 {
		return nil
	}
	perCycle := CPUPerCycleMs(p)
	if perCycle <= b.SidecarCPUMsPerCycle {
		return nil
	}
	return []Offender{{Gate: GateSidecarCPU, Phase: p.Name, Subject: "sidecar process",
		Measured: perCycle, Budget: b.SidecarCPUMsPerCycle, Unit: "ms per cycle",
		Detail: fmt.Sprintf("%.0f ms CPU over %d cycles", float64(p.ProcessCPU)/
			float64(time.Millisecond), p.Cycles)}}
}

// CPUPerCycleMs is a phase's sidecar CPU per collector cycle.
func CPUPerCycleMs(p Phase) float64 {
	if p.Cycles <= 0 {
		return 0
	}
	return float64(p.ProcessCPU) / float64(time.Millisecond) / float64(p.Cycles)
}

func statementSubject(s Statement) string {
	return fmt.Sprintf("queryid %d: %s", s.QueryID, shortQuery(s.Query))
}

// catalogRelation matches a reference to a system catalog or statistics
// view (pg_class, pg_stat_*, pg_catalog.*, information_schema.*).
var catalogRelation = regexp.MustCompile(`(?i)\bpg_catalog\.|\binformation_schema\.|` +
	`\bpg_(class|index|indexes|namespace|attribute|attrdef|constraint|sequences?|locks|` +
	`settings|database|roles|authid|tables|tablespace|inherits|partitioned_table|proc|` +
	`type|extension|replication_slots|prepared_xacts|depend|trigger|description|am|` +
	`opclass|views|matviews|shdepend|publication|subscription|stat[a-z_]*)\b`)

// IsCatalogQuery reports whether a statement reads the system catalog or
// the statistics views. A matched name followed by "(" is a function call
// (pg_stat_statements_reset(), pg_class_aux()), not a relation.
func IsCatalogQuery(q string) bool {
	for _, m := range catalogRelation.FindAllStringIndex(q, -1) {
		rest := strings.TrimLeft(q[m[1]:], " \t\n")
		if strings.HasSuffix(q[m[0]:m[1]], ".") || !strings.HasPrefix(rest, "(") {
			return true
		}
	}
	return false
}

var spaces = regexp.MustCompile(`\s+`)

// shortQuery is a statement on one line, at most 160 characters.
func shortQuery(q string) string {
	q = strings.TrimSpace(spaces.ReplaceAllString(q, " "))
	if len(q) > 160 {
		return q[:160] + "..."
	}
	return q
}

// scans reports whether a plan's sequential scan reads table or one of its
// partitions (<table>_history, <table>_pYYYYMMDD, <table>_default).
func scans(s PlanScan, table string) bool {
	name := s.Schema + "." + s.Relation
	return name == table || strings.HasPrefix(name, table+"_")
}
