package fleetlearn

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FindingRow is one database's open finding, the drill-down of a fleet
// finding.
type FindingRow struct {
	Database         string    `json:"database"`
	ID               int64     `json:"id"`
	Category         string    `json:"category"`
	Severity         string    `json:"severity"`
	ObjectIdentifier string    `json:"object_identifier"`
	Title            string    `json:"title"`
	Recommendation   string    `json:"recommendation,omitempty"`
	LastSeen         time.Time `json:"last_seen"`
}

// FleetFinding is one problem open on several databases of the fleet.
type FleetFinding struct {
	Key              string       `json:"key"`
	Category         string       `json:"category"`
	ObjectIdentifier string       `json:"object_identifier"`
	Title            string       `json:"title"`
	Severity         string       `json:"severity"`
	Databases        int          `json:"databases"`
	LastSeen         time.Time    `json:"last_seen"`
	Occurrences      []FindingRow `json:"occurrences"`
}

// MinFleetDatabases is the smallest recurrence that is a fleet finding.
const MinFleetDatabases = 2

func findingKey(r FindingRow) string {
	return strings.ToLower(strings.TrimSpace(r.Category)) + "|" +
		strings.ToLower(strings.TrimSpace(r.ObjectIdentifier))
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	}
	return 0
}

// GroupFleetFindings groups open findings by category and object and keeps
// those open on at least minDatabases distinct databases (minimum 2), worst
// severity first, then the most widespread.
func GroupFleetFindings(rows []FindingRow, minDatabases int) []FleetFinding {
	if minDatabases < MinFleetDatabases {
		minDatabases = MinFleetDatabases
	}
	groups := map[string][]FindingRow{}
	for _, r := range rows {
		if r.Database == "" {
			continue
		}
		k := findingKey(r)
		groups[k] = append(groups[k], r)
	}
	out := []FleetFinding{}
	for key, occ := range groups {
		if f, ok := fleetFinding(key, occ, minDatabases); ok {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra > rb
		}
		if a.Databases != b.Databases {
			return a.Databases > b.Databases
		}
		return a.Key < b.Key
	})
	return out
}

func fleetFinding(key string, occ []FindingRow, min int) (FleetFinding, bool) {
	dbs := map[string]bool{}
	for _, r := range occ {
		dbs[r.Database] = true
	}
	if len(dbs) < min {
		return FleetFinding{}, false
	}
	sort.Slice(occ, func(i, j int) bool {
		if occ[i].Database != occ[j].Database {
			return occ[i].Database < occ[j].Database
		}
		return occ[i].ID < occ[j].ID
	})
	f := FleetFinding{Key: key, Category: occ[0].Category,
		ObjectIdentifier: strings.TrimSpace(occ[0].ObjectIdentifier), Title: occ[0].Title,
		Databases: len(dbs), Occurrences: occ}
	for _, r := range occ {
		if severityRank(r.Severity) > severityRank(f.Severity) {
			f.Severity = r.Severity
		}
		if r.LastSeen.After(f.LastSeen) {
			f.LastSeen = r.LastSeen
		}
	}
	return f, true
}

// Source is one fleet database to read findings from.
type Source struct {
	Name string
	Pool *pgxpool.Pool
}

// SourceError is a database whose findings could not be read. Error is
// safe to show; Cause is for the log.
type SourceError struct {
	Database string `json:"database"`
	Error    string `json:"error"`
	Cause    error  `json:"-"`
}

// FleetFindingsResult is a fleet-wide read of recurring findings.
type FleetFindingsResult struct {
	Findings         []FleetFinding `json:"findings"`
	MinDatabases     int            `json:"min_databases"`
	DatabasesScanned int            `json:"databases_scanned"`
	Errors           []SourceError  `json:"errors"`
}

const (
	openFindingsSQL = `/* pg_sage */ SELECT id, category, severity,
	object_identifier, title, COALESCE(recommendation, ''), last_seen
	FROM sage.findings WHERE status = 'open'
	ORDER BY last_seen DESC LIMIT $1`
	findingsReadTimeout = 10 * time.Second
)

// CollectFleetFindings reads the open findings of every source (at most
// perDatabaseLimit each) and groups the recurring ones. A database that
// cannot be read is reported in Errors and never hides the others.
func CollectFleetFindings(ctx context.Context, sources []Source, minDatabases,
	perDatabaseLimit int) FleetFindingsResult {
	if minDatabases < MinFleetDatabases {
		minDatabases = MinFleetDatabases
	}
	res := FleetFindingsResult{MinDatabases: minDatabases, Errors: []SourceError{}}
	var rows []FindingRow
	for _, s := range sources {
		got, err := readOpenFindings(ctx, s, perDatabaseLimit)
		if err != nil {
			res.Errors = append(res.Errors, SourceError{Database: s.Name,
				Error: "read open findings failed", Cause: err})
			continue
		}
		res.DatabasesScanned++
		rows = append(rows, got...)
	}
	res.Findings = GroupFleetFindings(rows, minDatabases)
	return res
}

func readOpenFindings(ctx context.Context, s Source, limit int) ([]FindingRow, error) {
	if s.Pool == nil {
		return nil, fmt.Errorf("fleet findings: database %q has no connection", s.Name)
	}
	if limit <= 0 {
		limit = 500
	}
	ctx, cancel := context.WithTimeout(ctx, findingsReadTimeout)
	defer cancel()
	rows, err := s.Pool.Query(ctx, openFindingsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("fleet findings: read %q: %w", s.Name, err)
	}
	defer rows.Close()
	var out []FindingRow
	for rows.Next() {
		r := FindingRow{Database: s.Name}
		if err := rows.Scan(&r.ID, &r.Category, &r.Severity, &r.ObjectIdentifier,
			&r.Title, &r.Recommendation, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("fleet findings: scan %q: %w", s.Name, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet findings: read %q: %w", s.Name, err)
	}
	return out, nil
}

// Need is a database's measured demand for model attention.
type Need struct {
	OpenFindings     int
	CriticalFindings int
	ActiveIncidents  int
}

// MaxNeedWeight bounds NeedWeight so one database cannot claim the fleet.
const MaxNeedWeight = 1 + 10 + 40 + 40

// NeedWeight turns a need into a budget weight: 1 for an idle database,
// more for open findings, critical findings and active incidents (each
// capped).
func NeedWeight(n Need) float64 {
	clamp := func(v, max int) float64 {
		if v < 0 {
			return 0
		}
		if v > max {
			return float64(max)
		}
		return float64(v)
	}
	return 1 + clamp(n.OpenFindings, 100)/10 + 2*clamp(n.CriticalFindings, 20) +
		4*clamp(n.ActiveIncidents, 10)
}
