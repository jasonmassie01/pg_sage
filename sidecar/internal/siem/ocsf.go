// Package siem exports pg_sage's hash-chained audit to SIEM systems as OCSF
// events over HTTP, syslog (RFC 5424) and OTLP (E2, spec §6.17). The SIEM is
// the off-box copy of the audit: every event carries its chain link's hash,
// so a rewritten chain no longer matches what the SIEM already holds.
//
// Export reads the chain link table with a per-sink cursor, in chain order,
// on its own goroutines: audit writers never wait on it, a failing sink
// holds its cursor (backpressure in the database, not in memory) and gets
// every event at least once when it recovers.
package siem

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// OCSFVersion is the OCSF schema version the events follow.
const OCSFVersion = "1.3.0"

// OCSF classes used.
const (
	ClassAccountChange     = 3001
	ClassAuthentication    = 3002
	ClassEntityManagement  = 3004
	ClassAPIActivity       = 6003
	ClassDatastoreActivity = 6005
)

// OCSF severities used.
const (
	SeverityInformational = 1
	SeverityLow           = 2
	SeverityMedium        = 3
	SeverityHigh          = 4
)

// OCSF status ids.
const (
	statusUnknown = 0
	statusSuccess = 1
	statusFailure = 2
)

// MaxEventBytes bounds one encoded event so it fits a UDP syslog datagram.
const MaxEventBytes = 60000

var classNames = map[int]string{
	ClassAccountChange: "Account Change", ClassAuthentication: "Authentication",
	ClassEntityManagement: "Entity Management", ClassAPIActivity: "API Activity",
	ClassDatastoreActivity: "Datastore Activity",
}

var categoryNames = map[int]string{3: "Identity & Access Management",
	6: "Application Activity"}

var severityNames = map[int]string{SeverityInformational: "Informational",
	SeverityLow: "Low", SeverityMedium: "Medium", SeverityHigh: "High"}

var statusNames = map[int]string{statusUnknown: "Unknown", statusSuccess: "Success",
	statusFailure: "Failure"}

// Event is one OCSF event as a JSON object.
type Event map[string]any

// Record is one chain link with the current content of its row (nil when
// the row is gone).
type Record struct {
	Source string // database name, or "control"
	Chain  string
	Link   auditchain.Link
	Row    map[string]any
}

// mapping is what a chain-specific mapper decides about one record.
type mapping struct {
	class, activity, status, severity int
	activityName, message             string
	extra                             map[string]any
}

// Map renders r as an OCSF event.
func Map(r Record) Event {
	m := classify(r)
	e := Event{
		"class_uid": m.class, "class_name": classNames[m.class],
		"category_uid": m.class / 1000, "category_name": categoryNames[m.class/1000],
		"activity_id": m.activity, "activity_name": m.activityName,
		"type_uid":    m.class*100 + m.activity,
		"type_name":   classNames[m.class] + ": " + m.activityName,
		"severity_id": m.severity, "severity": severityNames[m.severity],
		"status_id": m.status, "status": statusNames[m.status],
		"time": r.Link.At.UnixMilli(), "message": m.message,
		"metadata": map[string]any{"version": OCSFVersion, "log_name": r.Chain,
			"uid":         r.Source + "/" + r.Chain + "/" + strconv.FormatInt(r.Link.Seq, 10),
			"logged_time": r.Link.At.UnixMilli(),
			"product":     map[string]any{"name": "pg_sage", "vendor_name": "pg_sage"}},
		"actor": map[string]any{"app_name": r.Link.App,
			"user": map[string]any{"name": r.Link.DBUser}},
		"unmapped": map[string]any{"pg_sage": chainProof(r)},
	}
	for k, v := range m.extra {
		e[k] = v
	}
	fit(e)
	return e
}

// chainProof is the link itself, so the SIEM can re-verify the chain.
func chainProof(r Record) map[string]any {
	return map[string]any{"source": r.Source, "chain": r.Chain, "seq": r.Link.Seq,
		"op": r.Link.Op, "row_id": r.Link.RowID, "v": r.Link.V, "hash": r.Link.Hash,
		"prev_hash": r.Link.PrevHash, "sealed_hash": r.Link.SealedHash,
		"state": r.Link.State, "row": sanitizeRow(redactRow(r))}
}

func classify(r Record) mapping {
	if r.Link.Op == "D" || r.Link.Op == "T" {
		return trailRemoval(r)
	}
	switch r.Chain {
	case auditchain.ActionLog.Chain:
		return actionMapping(r)
	case auditchain.AuthAudit.Chain:
		return authMapping(r)
	case auditchain.ConfigAudit.Chain:
		return configMapping(r)
	case auditchain.GuardQueryAudit.Chain:
		return queryAuditMapping(r)
	case auditchain.PGAuditEvents.Chain:
		return pgauditMapping(r)
	}
	return mapping{class: ClassAPIActivity, activity: 99, activityName: "Other",
		status: statusSuccess, severity: SeverityInformational,
		message: "pg_sage audit event (" + r.Chain + ")"}
}

// chainClass is the class a chain's ordinary events use.
func chainClass(chain string) int {
	switch chain {
	case auditchain.ActionLog.Chain, auditchain.GuardQueryAudit.Chain,
		auditchain.PGAuditEvents.Chain:
		return ClassDatastoreActivity
	case auditchain.AuthAudit.Chain:
		return ClassAuthentication
	case auditchain.ConfigAudit.Chain:
		return ClassEntityManagement
	}
	return ClassAPIActivity
}

// trailRemoval reports an audit row leaving the trail: pg_sage's own
// retention is routine, anyone else's delete is worth a look, and a
// truncate is high severity.
func trailRemoval(r Record) mapping {
	m := mapping{class: chainClass(r.Chain), activity: 99, status: statusSuccess,
		severity: SeverityInformational, activityName: "Audit Record Deleted",
		message: fmt.Sprintf("audit record %d of %s deleted by %s (%s)", r.Link.RowID,
			r.Chain, r.Link.DBUser, r.Link.App)}
	if !strings.HasPrefix(r.Link.App, "pg_sage") {
		m.severity = SeverityMedium
	}
	if r.Link.Op == "T" {
		m.activityName, m.severity = "Audit Trail Truncated", SeverityHigh
		m.message = fmt.Sprintf("%s truncated by %s (%s)", r.Chain, r.Link.DBUser,
			r.Link.App)
	}
	return m
}

func str(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	switch v := row[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func databaseObject(source string) map[string]any {
	return map[string]any{"name": source, "type": "Relational", "type_id": 1}
}
