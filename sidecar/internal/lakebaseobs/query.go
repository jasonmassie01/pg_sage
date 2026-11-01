package lakebaseobs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/databricks/databricks-sdk-go/service/sql"
)

type streamSpec struct {
	columns  []string
	private  map[string]bool
	dbColumn string
	dbName   bool
	compute  bool
}

func spec(columns, private, db string, name, compute bool) streamSpec {
	p := map[string]bool{}
	for _, s := range strings.Fields(private) {
		p[s] = true
	}
	return streamSpec{strings.Fields("project_id branch_id endpoint_id compute_id ts compute_mode " + columns), p, db, name, compute}
}

// Fixed public-schema projections deliberately fail on changed required columns.
// Missing optional provider columns must be added through a reviewed version.
var specs = map[Stream]streamSpec{
	Plans:             spec("row_position queryid dbid datname application_name backend_pid query_text plan_json total_cost duration_ms plan_fingerprint analyze_enabled buffers_enabled truncated parameters", "query_text plan_json parameters", "dbid", false, false),
	Logs:              spec("sample_seq datid datname usename application_name backend_pid severity sqlstate message detail hint context statement truncated_mask", "message detail hint context statement", "datid", false, false),
	ASH:               spec("sample_seq datid pid queryid blocking_pid leader_pid wait_event_type wait_event xact_age_ms query_age_ms plan_fingerprint appname_hash", "", "datid", false, false),
	DDL:               spec("row_position dbid application_name backend_pid command_tag object_type object_name schema_name usename query_text ddl_json duration_ms", "query_text ddl_json", "dbid", false, false),
	ComputeCounters:   spec("previous_ts host_cpu_seconds host_busiest_cpu_seconds host_disk_read_bytes host_disk_written_bytes backpressure_throttling_seconds", "", "", false, true),
	ComputeGauges:     spec("host_logical_cpus host_memory_total_bytes max_connections lfc_size_limit lfc_used total_size_limit total_size replication_lag_time_ms", "", "", false, true),
	DatabaseCounters:  spec("previous_ts datid datname xact_commit xact_rollback deadlocks rows_inserted rows_updated rows_deleted sessions_established sessions_fatal", "", "datid", false, false),
	DatabaseGauges:    spec("datid datname numbackends size oldest_frozen_xid_age oldest_mxid_age", "", "datid", false, false),
	WaitCounters:      spec("previous_ts wait_event_id wait_class_name wait_event_name wait_event_count wait_event_time", "", "", false, true),
	StatementCounters: spec("previous_ts dbid userid queryid query calls total_exec_time total_plan_time rows toplevel", "query", "dbid", false, false),
}

func checkDatabase(stream Stream, s Scope, f map[string]string) error {
	p, ok := specs[stream]
	if !ok {
		return ErrSchema
	}
	if p.compute {
		if !s.AllowCompute {
			return fmt.Errorf("%w: endpoint-wide telemetry not authorized", ErrScope)
		}
		return nil
	}
	v, e := strconv.ParseInt(f[p.dbColumn], 10, 64)
	if e != nil || v != s.DatabaseOID {
		return ErrScope
	}
	if name := f["datname"]; name != "" && name != s.DatabaseName {
		return ErrScope
	}
	if s.ApplicationName != "" && (stream == Plans || stream == Logs || stream == DDL) && f["application_name"] != s.ApplicationName {
		return ErrScope
	}
	return nil
}

func query(stream Stream, scope Scope, w Window, o Options) (string, []sql.StatementParameterListItem, []string, error) {
	if err := scope.Validate(); err != nil {
		return "", nil, nil, err
	}
	if err := w.Validate(); err != nil {
		return "", nil, nil, err
	}
	p, ok := specs[stream]
	if !ok {
		return "", nil, nil, ErrSchema
	}
	if p.compute && !scope.AllowCompute {
		return "", nil, nil, ErrScope
	}
	applicationScoped := stream == Plans || stream == Logs || stream == DDL
	if scope.ApplicationName != "" && o.AllowPayload && len(p.private) > 0 && !applicationScoped {
		return "", nil, nil, ErrScope
	}
	projection := make([]string, 0, len(p.columns))
	for _, col := range p.columns {
		if col == "ts" || col == "previous_ts" {
			// CAST(timestamp AS STRING) omits the session timezone. Preserve an
			// explicit offset so non-UTC warehouses cannot corrupt event age.
			projection = append(projection, "date_format(`"+col+"`, 'yyyy-MM-dd HH:mm:ss.SSSSSSXXX') AS `"+col+"`")
		} else if p.private[col] && !o.AllowPayload {
			projection = append(projection, "CAST(NULL AS STRING) AS `"+col+"`")
		} else {
			projection = append(projection, "CAST(`"+col+"` AS STRING) AS `"+col+"`")
		}
	}
	object := "system.lakebase." + string(stream)
	if configured := o.SourceObjects[stream]; configured != "" {
		object = configured
	}
	quoted, err := sourceObject(object)
	if err != nil {
		return "", nil, nil, err
	}
	stmt := "/* pg_sage lakebaseobs v1 */ SELECT " + strings.Join(projection, ", ") + " FROM " + quoted + " WHERE project_id = :project AND branch_id = :branch AND endpoint_id = :endpoint AND ts >= CAST(:from AS TIMESTAMP) AND ts < CAST(:to AS TIMESTAMP)"
	params := []sql.StatementParameterListItem{{Name: "project", Value: scope.ProjectID}, {Name: "branch", Value: scope.BranchID}, {Name: "endpoint", Value: scope.EndpointID}, {Name: "from", Value: w.From.UTC().Format("2006-01-02T15:04:05.999999999Z")}, {Name: "to", Value: w.To.UTC().Format("2006-01-02T15:04:05.999999999Z")}}
	if p.dbColumn != "" {
		stmt += " AND `" + p.dbColumn + "` = :database_oid"
		params = append(params, sql.StatementParameterListItem{Name: "database_oid", Type: "BIGINT", Value: strconv.FormatInt(scope.DatabaseOID, 10)})
	}
	if scope.ApplicationName != "" && applicationScoped {
		stmt += " AND application_name = :application"
		params = append(params, sql.StatementParameterListItem{Name: "application", Value: scope.ApplicationName})
	}
	stmt += " ORDER BY ts, compute_id LIMIT " + strconv.Itoa(o.MaxRows+1)
	return stmt, params, p.columns, nil
}

var objectPart = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,254}$`)

func sourceObject(object string) (string, error) {
	parts := strings.Split(object, ".")
	if len(parts) != 3 {
		return "", ErrSchema
	}
	for i, p := range parts {
		if !objectPart.MatchString(p) {
			return "", ErrSchema
		}
		parts[i] = "`" + p + "`"
	}
	return strings.Join(parts, "."), nil
}
