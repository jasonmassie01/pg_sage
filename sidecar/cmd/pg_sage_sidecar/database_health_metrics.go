package main

import (
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/policy"
)

// observability-02: per-database health, rendered in every mode from the
// fleet manager's in-memory status (FleetStatus does no database I/O).
// Standalone registers its one database in the manager too, so all modes
// share this path; the metadata database is never one of the rows.

// healthFamily is one gauge family (help in internal/metricsreg); write
// renders one database's samples.
type healthFamily struct {
	name  string
	write func(b *strings.Builder, family string, db fleet.DatabaseStatus)
}

var trustLevels = []string{policy.TrustObservation, policy.TrustAdvisory, policy.TrustAutonomous}

var databaseHealthFamilies = []healthFamily{
	{"pg_sage_database_up", writeDatabaseUp},
	{"pg_sage_collector_last_success_timestamp_seconds", writeCollectorSuccess},
	{"pg_sage_analyzer_last_success_timestamp_seconds", writeAnalyzerSuccess},
	{"pg_sage_findings_open", writeFindingsOpen},
	{"pg_sage_database_health_score", writeHealthScore},
	{"pg_sage_emergency_stopped", writeEmergencyStopped},
	{"pg_sage_trust_level",
		writeTrustLevel},
	{"pg_sage_database_size_bytes", writeDatabaseSize},
}

// writeDatabaseHealthMetrics renders every health family; a nil manager or
// one without databases gives HELP/TYPE only.
func writeDatabaseHealthMetrics(b *strings.Builder, mgr *fleet.DatabaseManager) {
	rows := databaseHealthRows(mgr)
	for _, f := range databaseHealthFamilies {
		metricsreg.Header(b, f.name)
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
	writeSample(b, family, dbLabel(db.Name), up)
}

func writeCollectorSuccess(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	if at := db.Status.CollectorLastRun; !at.IsZero() {
		writeSample(b, family, dbLabel(db.Name), at.Unix())
	}
}

func writeAnalyzerSuccess(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	if at := db.Status.AnalyzerLastRun; !at.IsZero() {
		writeSample(b, family, dbLabel(db.Name), at.Unix())
	}
}

func writeFindingsOpen(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	s := db.Status
	for _, sev := range []struct {
		name  string
		count int
	}{{"critical", s.FindingsCritical}, {"warning", s.FindingsWarning},
		{"info", s.FindingsInfo}} {
		writeSample(b, family, []labelPair{{"database", db.Name}, {"severity", sev.name}},
			sev.count)
	}
}

func writeHealthScore(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	writeSample(b, family, dbLabel(db.Name), db.Status.HealthScore)
}

func writeEmergencyStopped(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	stopped := 0
	if db.EmergencyStopped {
		stopped = 1
	}
	writeSample(b, family, dbLabel(db.Name), stopped)
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
		writeSample(b, family, []labelPair{{"database", db.Name}, {"level", l}}, v)
	}
	if !known {
		writeSample(b, family, []labelPair{{"database", db.Name}, {"level", level}}, 1)
	}
}

// writeDatabaseSize omits an unknown size (never measured) rather than
// reporting 0; the value is the collector's cached measurement.
func writeDatabaseSize(b *strings.Builder, family string, db fleet.DatabaseStatus) {
	if size := db.Status.DatabaseSize; size > 0 {
		writeSample(b, family, dbLabel(db.Name), size)
	}
}
