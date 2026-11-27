package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-06 unit tests: the maintenance-privilege precheck decides from one
// catalog read (plus the MAINTAIN read on PostgreSQL 17+ only) whether the
// connected role may VACUUM or ALTER TABLE ... SET a table.

// No concurrent access tests: the precheck is one read before each action,
// under the action's lease, and keeps no state between calls.

// fakeRow is one canned row (or error) of fakeQuerier.
type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("fake row: scan %d columns into %d", len(r.values), len(dest))
	}
	for i, v := range r.values {
		switch d := dest[i].(type) {
		case *bool:
			*d = v.(bool)
		case *int:
			*d = v.(int)
		case *string:
			*d = v.(string)
		default:
			return fmt.Errorf("fake row: unsupported destination %T", dest[i])
		}
	}
	return nil
}

// fakeQuerier answers QueryRow from rows in order and records every SQL.
type fakeQuerier struct {
	rows []fakeRow
	sqls []string
	args [][]any
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.sqls = append(q.sqls, sql)
	q.args = append(q.args, args)
	if len(q.rows) == 0 {
		return fakeRow{err: errors.New("fake querier: unexpected query")}
	}
	row := q.rows[0]
	q.rows = q.rows[1:]
	return row
}

func (q *fakeQuerier) issuedMaintain() bool {
	for _, sql := range q.sqls {
		if strings.Contains(sql, "MAINTAIN") || strings.Contains(sql, "pg_maintain") {
			return true
		}
	}
	return false
}

// baseRow is the base catalog read: exists, owner, superuser, database
// owner, server_version_num, current_user.
func baseRow(exists, owner, super, dbOwner bool, version int) fakeRow {
	return fakeRow{values: []any{exists, owner, super, dbOwner, version, "app_role"}}
}

func TestMaintenancePrivilegeOwnerAllowedInOneRead(t *testing.T) {
	q := &fakeQuerier{rows: []fakeRow{baseRow(true, true, false, false, 170004)}}
	got, err := readMaintenancePrivilege(context.Background(), q, "public.t", "vacuum_table")
	if err != nil {
		t.Fatalf("readMaintenancePrivilege: %v", err)
	}
	if !got.Allowed || got.Basis != "owner" || !got.Exists {
		t.Fatalf("privilege = %+v, want allowed as owner", got)
	}
	if len(q.sqls) != 1 || q.issuedMaintain() {
		t.Fatalf("queries = %q, want the one base read only", q.sqls)
	}
	if len(q.args[0]) != 1 || q.args[0][0] != "public.t" {
		t.Fatalf("args = %v, want the table as the only parameter", q.args[0])
	}
}

func TestMaintenancePrivilegeSuperuserAllowed(t *testing.T) {
	for _, action := range []string{"vacuum_table", "set_table_autovacuum"} {
		q := &fakeQuerier{rows: []fakeRow{baseRow(true, false, true, false, 140010)}}
		got, err := readMaintenancePrivilege(context.Background(), q, "public.t", action)
		if err != nil || !got.Allowed || got.Basis != "superuser" {
			t.Fatalf("%s: privilege = %+v, %v; want allowed as superuser", action, got, err)
		}
	}
}

func TestMaintenancePrivilegeDatabaseOwnerVacuumOnly(t *testing.T) {
	q := &fakeQuerier{rows: []fakeRow{baseRow(true, false, false, true, 160002)}}
	got, err := readMaintenancePrivilege(context.Background(), q, "public.t", "vacuum_table")
	if err != nil || !got.Allowed || got.Basis != "database_owner" {
		t.Fatalf("vacuum: privilege = %+v, %v; want allowed as database owner", got, err)
	}
	q = &fakeQuerier{rows: []fakeRow{baseRow(true, false, false, true, 170000)}}
	got, err = readMaintenancePrivilege(context.Background(), q, "public.t",
		"set_table_autovacuum")
	if err != nil {
		t.Fatalf("alter: %v", err)
	}
	if got.Allowed || !strings.Contains(got.Detail, "ownership required") {
		t.Fatalf("alter: privilege = %+v, want refused: ownership required", got)
	}
	if q.issuedMaintain() {
		t.Fatalf("alter issued a MAINTAIN read: %q", q.sqls)
	}
}

// Boundary: below PostgreSQL 17 the MAINTAIN privilege does not exist and
// naming it fails with "unrecognized privilege type", so it is never read.
func TestMaintenancePrivilegeNoMaintainReadBelow17(t *testing.T) {
	for _, version := range []int{140000, 160000, 169999} {
		q := &fakeQuerier{rows: []fakeRow{baseRow(true, false, false, false, version)}}
		got, err := readMaintenancePrivilege(context.Background(), q, "public.t",
			"vacuum_table")
		if err != nil {
			t.Fatalf("%d: %v", version, err)
		}
		if got.Allowed || !strings.Contains(got.Detail, "ownership required") {
			t.Fatalf("%d: privilege = %+v, want refused: ownership required", version, got)
		}
		if strings.Contains(got.Detail, "MAINTAIN") {
			t.Fatalf("%d: detail %q names MAINTAIN, which this version lacks",
				version, got.Detail)
		}
		if len(q.sqls) != 1 || q.issuedMaintain() {
			t.Fatalf("%d: queries = %q, want no MAINTAIN read", version, q.sqls)
		}
	}
}

func TestMaintenancePrivilegeMaintainReadFrom17(t *testing.T) {
	q := &fakeQuerier{rows: []fakeRow{baseRow(true, false, false, false, 170000),
		{values: []any{true}}}}
	got, err := readMaintenancePrivilege(context.Background(), q, "public.t", "vacuum_table")
	if err != nil || !got.Allowed || got.Basis != "maintain" {
		t.Fatalf("privilege = %+v, %v; want allowed by MAINTAIN", got, err)
	}
	if len(q.sqls) != 2 || !q.issuedMaintain() {
		t.Fatalf("queries = %q, want the base read and the MAINTAIN read", q.sqls)
	}
}

func TestMaintenancePrivilegeMissingMaintainNamesGrant(t *testing.T) {
	q := &fakeQuerier{rows: []fakeRow{baseRow(true, false, false, false, 180001),
		{values: []any{false}}}}
	got, err := readMaintenancePrivilege(context.Background(), q, "public.t", "vacuum_table")
	if err != nil {
		t.Fatalf("readMaintenancePrivilege: %v", err)
	}
	if got.Allowed || !strings.Contains(got.Detail, "GRANT MAINTAIN ON TABLE public.t TO") ||
		!strings.Contains(got.Detail, "app_role") || !strings.Contains(got.Detail, "pg_maintain") {
		t.Fatalf("privilege = %+v, want refused with the MAINTAIN grant to app_role", got)
	}
}

// A missing table is not a privilege gap: the action runs and fails with
// PostgreSQL's own "does not exist" error, recorded as a failed action.
func TestMaintenancePrivilegeMissingTablePassesThrough(t *testing.T) {
	q := &fakeQuerier{rows: []fakeRow{baseRow(false, false, false, false, 170000)}}
	got, err := readMaintenancePrivilege(context.Background(), q, "public.gone",
		"vacuum_table")
	if err != nil || !got.Allowed || got.Exists {
		t.Fatalf("privilege = %+v, %v; want allowed with Exists=false", got, err)
	}
	if len(q.sqls) != 1 {
		t.Fatalf("queries = %q, want only the base read", q.sqls)
	}
}

// Nil/empty: a statement naming no table cannot be checked; it is refused
// without a query.
func TestMaintenancePrivilegeEmptyTableRefused(t *testing.T) {
	q := &fakeQuerier{}
	got, err := readMaintenancePrivilege(context.Background(), q, "  ", "vacuum_table")
	if err != nil {
		t.Fatalf("readMaintenancePrivilege: %v", err)
	}
	if got.Allowed || got.Detail == "" || len(q.sqls) != 0 {
		t.Fatalf("privilege = %+v, queries %q; want refused without a query", got, q.sqls)
	}
}

// Error propagation: a failed catalog read is returned with what was read.
func TestMaintenancePrivilegeReadErrorsPropagate(t *testing.T) {
	cause := errors.New("connection refused")
	q := &fakeQuerier{rows: []fakeRow{{err: cause}}}
	got, err := readMaintenancePrivilege(context.Background(), q, "public.t", "vacuum_table")
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "public.t") {
		t.Fatalf("error = %v, want the cause naming public.t", err)
	}
	if got.Allowed {
		t.Fatalf("privilege = %+v, want not allowed on a read error", got)
	}
	q = &fakeQuerier{rows: []fakeRow{baseRow(true, false, false, false, 170000),
		{err: cause}}}
	got, err = readMaintenancePrivilege(context.Background(), q, "public.t", "vacuum_table")
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "MAINTAIN") || got.Allowed {
		t.Fatalf("maintain read: privilege = %+v, error %v; want the MAINTAIN cause",
			got, err)
	}
}

// Invalid input: only VACUUM and ALTER TABLE ... SET (autovacuum_*) are
// prechecked; any other statement is admitted untouched.
func TestAdmitMaintenanceIgnoresOtherActions(t *testing.T) {
	e := &Executor{logFn: nopLog}
	for _, sql := range []string{"ANALYZE public.t", "VACUUM FULL public.t",
		"CREATE INDEX CONCURRENTLY i ON public.t (a)", ""} {
		if err := e.admitMaintenance(context.Background(), sql,
			ActionPolicyDecision{Decision: PolicyDecisionExecute}); err != nil {
			t.Fatalf("%q: admitMaintenance = %v, want nil", sql, err)
		}
	}
}

// Fail closed: with no database to read the catalog from, a VACUUM is
// withheld, never run.
func TestAdmitMaintenanceWithoutPoolWithholds(t *testing.T) {
	e := &Executor{logFn: nopLog}
	err := e.admitMaintenance(context.Background(), "VACUUM public.t",
		ActionPolicyDecision{Decision: PolicyDecisionExecute})
	var withheld *WithheldError
	if !errors.As(err, &withheld) || !errors.Is(err, ErrActionWithheld) {
		t.Fatalf("error = %v, want a *WithheldError", err)
	}
	if withheld.Decision.Decision != PolicyDecisionBlocked ||
		withheld.Decision.BlockedReason != reasonCapabilityUnverified {
		t.Fatalf("decision = %+v, want blocked %s", withheld.Decision,
			reasonCapabilityUnverified)
	}
}

// Through Apply: a withheld precheck stops the action before Execute.
func TestApplyWithheldPrecheckNeverExecutes(t *testing.T) {
	e := &Executor{logFn: nopLog}
	executed := false
	_, err := e.Apply(context.Background(), ActionIntent{
		Request: policy.ActionRequest{
			SQL: "ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05)"},
		Authorize: func(context.Context) (ActionPolicyDecision, error) {
			return ActionPolicyDecision{Decision: PolicyDecisionExecute, RiskTier: "moderate"},
				nil
		},
		SlotHeld: true,
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) {
			executed = true
			return 1, nil
		},
	})
	if executed {
		t.Fatal("Execute ran although the maintenance precheck could not be evaluated")
	}
	if !errors.Is(err, ErrActionWithheld) {
		t.Fatalf("Apply error = %v, want withheld", err)
	}
}

func TestVacuumSkipped(t *testing.T) {
	cases := []struct {
		before, after float64
		want          bool
	}{
		{3, 3, true},   // did not advance
		{3, 4, false},  // advanced
		{0, 0, true},   // never vacuumed, still not
		{3, -1, false}, // unreadable: no claim
		{-1, 2, false}, // no baseline: no claim
		{5, 2, true},   // statistics reset: no advance seen; the privilege re-check decides
	}
	for _, tc := range cases {
		if got := vacuumSkipped(tc.before, tc.after); got != tc.want {
			t.Errorf("vacuumSkipped(%v, %v) = %v, want %v", tc.before, tc.after, got, tc.want)
		}
	}
}

// An operator's VACUUM withheld for a missing privilege is answered 409 with
// the grant that fixes it; a precheck that could not be read stays 500.
func TestManualExecuteStatusForMaintenancePrivilege(t *testing.T) {
	e := &Executor{logFn: nopLog}
	detail := "grant MAINTAIN: GRANT MAINTAIN ON TABLE public.t TO app_role"
	missing := e.withholdMaintenance(context.Background(),
		ActionPolicyDecision{Decision: PolicyDecisionExecute}, reasonCapabilityMissing, detail,
		ErrMaintenancePrivilegeMissing)
	var withheld *WithheldError
	if !errors.As(missing, &withheld) || !errors.Is(missing, ErrActionWithheld) {
		t.Fatalf("missing = %v, want a *WithheldError", missing)
	}
	if status := ManualExecuteStatus(missing); status != 409 {
		t.Fatalf("status for a missing privilege = %d, want 409", status)
	}
	if !strings.Contains(missing.Error(), detail) {
		t.Fatalf("message %q does not name the grant", missing.Error())
	}
	unverified := e.admitMaintenance(context.Background(), "VACUUM public.t",
		ActionPolicyDecision{Decision: PolicyDecisionExecute})
	if errors.Is(unverified, ErrMaintenancePrivilegeMissing) ||
		ManualExecuteStatus(unverified) != 500 {
		t.Fatalf("unverified = %v (status %d), want 500 without the missing marker",
			unverified, ManualExecuteStatus(unverified))
	}
}
