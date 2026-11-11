package action

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Statistics maintenance the morning after (playbooks-06). stat_tables
// evidence ages past MaxEvidenceAge long before an operator reviews a
// finished investigation, and a finished run has no lease to collect
// more. RefreshTableEvidence re-reads stat_tables once, under the pinned
// read policy, for a table the run already observed, and records it as
// operator_refresh evidence; the 15-minute cap, the lock-time identity
// rechecks and human approval are unchanged.

// RefreshInterval is the shortest gap between two refreshes of one
// investigation.
const RefreshInterval = time.Minute

// TableRefresh names the cited stat_tables evidence and the table to re-read.
type TableRefresh struct {
	EvidenceID sre.UUID
	Schema     string
	Table      string
}

// TableStatistics is a statistics request whose evidence the server
// resolves: the newest retained stat_tables row naming the table.
type TableStatistics struct {
	Schema    string
	Table     string
	Columns   []string
	Kinds     []string
	Rationale string
}

// RefreshTableEvidence re-reads stat_tables for a table the cited
// evidence names, with the same database and table OIDs, and records it.
func (m *MaintenanceService) RefreshTableEvidence(ctx context.Context, invID sre.UUID,
	req TableRefresh, actor string) (sre.Evidence, error) {
	if err := m.allowed(); err != nil {
		return sre.Evidence{}, err
	}
	if actor == "" || len(actor) > 128 {
		return sre.Evidence{}, fmt.Errorf("%w: refresh needs an actor", sre.ErrInvalidRequest)
	}
	scope, err := m.scope(ctx)
	if err != nil {
		return sre.Evidence{}, err
	}
	if err = m.reviewable(ctx, scope, invID); err != nil {
		return sre.Evidence{}, err
	}
	relation, err := m.relationName(ctx, req.Schema, req.Table)
	if err != nil {
		return sre.Evidence{}, err
	}
	cited, err := m.st.EvidenceByID(ctx, scope, invID, req.EvidenceID)
	if err != nil {
		return sre.Evidence{}, err
	}
	dbOID, tableOID, err := citedIdentity(cited, relation)
	if err != nil {
		return sre.Evidence{}, err
	}
	// A rate-limited call reads nothing on the monitored database.
	if err = m.st.RefreshAllowed(ctx, scope, invID, RefreshInterval); err != nil {
		return sre.Evidence{}, err
	}
	fresh := m.Service.Coordinator().ReadProbe(ctx, probes.StatTables, probes.Args{})
	if fresh.ProbeID != probes.StatTables || fresh.Status != probes.StatusOK {
		return sre.Evidence{}, fmt.Errorf("%w: stat_tables refresh returned %s %s",
			sre.ErrMetadataUnavailable, fresh.Status, fresh.Reason)
	}
	rows, err := resultRows(fresh)
	if err != nil {
		return sre.Evidence{}, err
	}
	if !hasTableIdentity(rows, relation, dbOID, tableOID) {
		return sre.Evidence{}, fmt.Errorf("%w: %s is no longer observed with the cited "+
			"database and table OIDs; start a new investigation", sre.ErrInvalidRequest,
			relation)
	}
	return m.st.AppendRefreshEvidence(ctx, scope, invID, actor, sre.RefreshRequest{
		SourceEvidenceID: cited.ID, Result: fresh, MinInterval: RefreshInterval})
}

// RefreshTable refreshes the newest retained stat_tables evidence that
// names the table.
func (m *MaintenanceService) RefreshTable(ctx context.Context, invID sre.UUID,
	schema, table, actor string) (sre.Evidence, error) {
	ev, err := m.NewestTableEvidence(ctx, invID, schema, table)
	if err != nil {
		return sre.Evidence{}, err
	}
	return m.RefreshTableEvidence(ctx, invID, TableRefresh{EvidenceID: ev.ID,
		Schema: schema, Table: table}, actor)
}

// ProposeStatisticsForTable proposes against the newest retained
// stat_tables evidence that names the table; the caller never supplies
// an evidence id or hash.
func (m *MaintenanceService) ProposeStatisticsForTable(ctx context.Context, invID sre.UUID,
	ts TableStatistics, actor string) (MaintenanceProposal, error) {
	ev, err := m.NewestTableEvidence(ctx, invID, ts.Schema, ts.Table)
	if err != nil {
		return MaintenanceProposal{}, err
	}
	return m.ProposeStatistics(ctx, invID, StatisticsRequest{Schema: ts.Schema,
		Table: ts.Table, Columns: ts.Columns, Kinds: ts.Kinds, EvidenceID: ev.ID,
		EvidenceSHA256: hex.EncodeToString(ev.SHA256), Rationale: ts.Rationale}, actor)
}

// NewestTableEvidence is the newest hash-verified stat_tables evidence of
// the investigation that names the table with its database and table OIDs.
func (m *MaintenanceService) NewestTableEvidence(ctx context.Context, invID sre.UUID,
	schema, table string) (sre.Evidence, error) {
	scope, err := m.scope(ctx)
	if err != nil {
		return sre.Evidence{}, err
	}
	relation, err := m.relationName(ctx, schema, table)
	if err != nil {
		return sre.Evidence{}, err
	}
	all, err := m.st.Evidence(ctx, scope, invID)
	if err != nil {
		return sre.Evidence{}, err
	}
	var newest *sre.Evidence
	for i := range all {
		e := &all[i]
		if e.CapabilityState != "available" {
			continue
		}
		if _, _, err := citedIdentity(*e, relation); err != nil {
			continue
		}
		if newest == nil || !e.ObservedAt.Before(newest.ObservedAt) {
			newest = e
		}
	}
	if newest == nil {
		return sre.Evidence{}, fmt.Errorf("%w: no retained stat_tables evidence names %s "+
			"with its table identity", sre.ErrNotFound, relation)
	}
	return *newest, nil
}

// relationName is the table's name as stat_tables writes it
// (quote_ident(schema).quote_ident(table)); it reads no catalog.
func (m *MaintenanceService) relationName(ctx context.Context, schema, table string) (string,
	error) {
	if schema == "" || table == "" || len(schema) > 63 || len(table) > 63 {
		return "", fmt.Errorf("%w: schema and table are 1 to 63 bytes", sre.ErrInvalidRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var relation string
	err := m.Pool.QueryRow(ctx, `/* pg_sage */ SELECT pg_catalog.quote_ident($1) || '.' ||
		pg_catalog.quote_ident($2)`, schema, table).Scan(&relation)
	if err != nil {
		return "", fmt.Errorf("%w: quote table name: %v", sre.ErrMetadataUnavailable, err)
	}
	return relation, nil
}

// citedIdentity is the database and table OIDs a verified stat_tables row
// gives the relation.
func citedIdentity(e sre.Evidence, relation string) (float64, float64, error) {
	if !e.VerifyHash() || e.ProbeID != string(probes.StatTables) {
		return 0, 0, fmt.Errorf("%w: refresh needs hash-verified stat_tables evidence",
			sre.ErrInvalidRequest)
	}
	rows, err := statTablesRows(e.Payload)
	if err != nil {
		return 0, 0, err
	}
	for _, row := range rows {
		dbOID, dbOK := row["database_oid"].(float64)
		tableOID, tableOK := row["table_oid"].(float64)
		if row["relation"] == relation && dbOK && tableOK {
			return dbOID, tableOID, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: the evidence does not name %s with its database and "+
		"table OIDs", sre.ErrInvalidRequest, relation)
}

// statTablesRows decodes a usable stat_tables payload's rows.
func statTablesRows(payload []byte) ([]probes.Row, error) {
	var res probes.Result
	if err := json.Unmarshal(payload, &res); err != nil {
		return nil, err
	}
	if res.ProbeID != probes.StatTables || res.Status != probes.StatusOK {
		return nil, sre.ErrInvalidRequest
	}
	return res.Rows, nil
}

// resultRows is a fresh result's rows as they will be stored (JSON numbers).
func resultRows(res probes.Result) ([]probes.Row, error) {
	raw, err := res.Payload()
	if err != nil {
		return nil, fmt.Errorf("%w: stat_tables payload: %v", sre.ErrInvalidRequest, err)
	}
	return statTablesRows(raw)
}

func hasTableIdentity(rows []probes.Row, relation string, dbOID, tableOID float64) bool {
	for _, row := range rows {
		gotDB, dbOK := row["database_oid"].(float64)
		gotTable, tableOK := row["table_oid"].(float64)
		if row["relation"] == relation && dbOK && tableOK && gotDB == dbOID &&
			gotTable == tableOID {
			return true
		}
	}
	return false
}
