package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
)

// observability-02: per-database health, rendered in every mode from the
// fleet manager's in-memory status (FleetStatus does no database I/O).
// Standalone registers its one database in the manager too, so all modes
// share this path; the metadata database is never one of the rows.

// healthFamily is one gauge family; write renders one database's samples.
type healthFamily struct {
	name, help string
	write      func(b *strings.Builder, family string, db fleet.DatabaseStatus)
}

var trustLevels = []string{policy.TrustObservation, policy.TrustAdvisory, policy.TrustAutonomous}

var databaseHealthFamilies = []healthFamily{
	{"pg_sage_database_up", "1 if the monitored database is connected and its last " +
		"status refresh succeeded", writeDatabaseUp},
	{"pg_sage_collector_last_success_timestamp_seconds", "Unix time of the " +
		"database's last completed collector snapshot", writeCollectorSuccess},
	{"pg_sage_analyzer_last_success_timestamp_seconds", "Unix time of the " +
		"database's last successful findings refresh after analysis", writeAnalyzerSuccess},
	{"pg_sage_findings_open", "Open findings by database and severity", writeFindingsOpen},
	{"pg_sage_database_health_score", "Database health score, 0-100 (0 while " +
		"disconnected or in error)", writeHealthScore},
	{"pg_sage_emergency_stopped", "1 while the database's emergency stop holds " +
		"(its own or the fleet-wide stop)", writeEmergencyStopped},
	{"pg_sage_trust_level", "Effective trust level of the database (one-hot)",
		writeTrustLevel},
	{"pg_sage_database_size_bytes", "Database size from the collector's last " +
		"snapshot", writeDatabaseSize},
}

// writeDatabaseHealthMetrics renders every health family; a nil manager or
// one without databases gives HELP/TYPE only.
func writeDatabaseHealthMetrics(b *strings.Builder, mgr *fleet.DatabaseManager) {
	rows := databaseHealthRows(mgr)
	for _, f := range databaseHealthFamilies {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", f.name, f.help, f.name)
		for _, db := range rows {
			f.write(b, f.name, db)
		}
		b.WriteString("\n")
	}
}

// databaseHealthRows is the manager's status by database name, so the
// exposition order is stable across scrapes.
func databaseHealthRows(mgr *fleet.DatabaseManager) []fleet.DatabaseStatus {
	if mgr == nil {
		return nil
	}
	all := mgr.FleetStatus().Databases
	rows := make([]fleet.DatabaseStatus, 0, len(all))
	for _, db := range all {
		if db.Status != nil {
			rows = append(rows, db)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

func writeDatabaseUp(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	up := 0
	if db.Status.Connected && db.Status.Error == "" {
		up = 1
	}
	fmt.Fprintf(b, "%s{database=%q} %d\n", family, db.Name, up)
}

func writeCollectorSuccess(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	if at := db.Status.CollectorLastRun; !at.IsZero() {
		fmt.Fprintf(b, "%s{database=%q} %d\n", family, db.Name, at.Unix())
	}
}

func writeAnalyzerSuccess(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	if at := db.Status.AnalyzerLastRun; !at.IsZero() {
		fmt.Fprintf(b, "%s{database=%q} %d\n", family, db.Name, at.Unix())
	}
}

func writeFindingsOpen(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	s := db.Status
	for _, sev := range []struct {
		name  string
		count int
	}{{"critical", s.FindingsCritical}, {"warning", s.FindingsWarning},
		{"info", s.FindingsInfo}} {
		fmt.Fprintf(b, "%s{database=%q,severity=%q} %d\n", family, db.Name, sev.name,
			sev.count)
	}
}

func writeHealthScore(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	fmt.Fprintf(b, "%s{database=%q} %d\n", family, db.Name, db.Status.HealthScore)
}

func writeEmergencyStopped(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	stopped := 0
	if db.EmergencyStopped {
		stopped = 1
	}
	fmt.Fprintf(b, "%s{database=%q} %d\n", family, db.Name, stopped)
}

// writeTrustLevel is one-hot over the known levels. No level yet (a
// database that never started) has no samples; an unrecognised level is
// shown as its own sample rather than hidden.
func writeTrustLevel(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	level := db.Status.TrustLevel
	if level == "" {
		return
	}
	known := false
	for _, l := range trustLevels {
		v := 0
		if l == level {
			v, known = 1, true
		}
		fmt.Fprintf(b, "%s{database=%q,level=%q} %d\n", family, db.Name, l, v)
	}
	if !known {
		fmt.Fprintf(b, "%s{database=%q,level=%q} 1\n", family, db.Name, level)
	}
}

// writeDatabaseSize omits an unknown size (never measured) rather than
// reporting 0; the value is the collector's cached measurement.
func writeDatabaseSize(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	if size := db.Status.DatabaseSize; size > 0 {
		fmt.Fprintf(b, "%s{database=%q} %d\n", family, db.Name, size)
	}
}
