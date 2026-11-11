package tuner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tuning-04: pg_hint_plan changes an application's plan only when the
// module is preloaded for its sessions, enable_hint_table is on for them,
// hint_plan.hints has the 1.7 layout and pg_sage may write it. These unit
// tests drive the readiness probe with scripted catalog rows.

type fakeHintRow struct {
	vals []any
	err  error
}

func (r fakeHintRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan %d values into %d targets", len(r.vals), len(dest))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case **string:
			*p = nil
			if s, ok := r.vals[i].(string); ok {
				*p = &s
			}
		case *bool:
			*p = r.vals[i].(bool)
		default:
			return fmt.Errorf("unsupported scan target %T", d)
		}
	}
	return nil
}

// fakeHintDB answers the readiness probe's two catalog reads and records
// every statement with whether its context carried a deadline.
type fakeHintDB struct {
	mu        sync.Mutex
	settings  fakeHintRow
	table     fakeHintRow
	sqls      []string
	deadlines []time.Duration
}

func (f *fakeHintDB) QueryRow(ctx context.Context, sql string, _ ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sqls = append(f.sqls, sql)
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	f.deadlines = append(f.deadlines, left)
	switch sql {
	case hintSettingsSQL:
		return f.settings
	case hintTableSQL:
		return f.table
	}
	return fakeHintRow{err: fmt.Errorf("unexpected statement %q", sql)}
}

func (f *fakeHintDB) set(settings, table fakeHintRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings, f.table = settings, table
}

const sharedWithHints = "pg_stat_statements,pg_hint_plan"

// settingsRow scripts the settings read: shared_preload_libraries (nil when
// unreadable), the server session_preload_libraries and enable_hint_table,
// and the pg_db_role_setting rows for this database and globally. The
// server's pg_hint_plan.enable_hint is at its default, on.
func settingsRow(shared any, session, tableOn string, roles ...roleSetting) fakeHintRow {
	return settingsRowHint(shared, session, tableOn, "on", roles...)
}

// settingsRowHint is settingsRow with the server's pg_hint_plan.enable_hint.
func settingsRowHint(shared any, session, tableOn, hintOn string,
	roles ...roleSetting) fakeHintRow {
	if roles == nil {
		roles = []roleSetting{}
	}
	doc, err := json.Marshal(roles)
	if err != nil {
		panic(err)
	}
	return fakeHintRow{vals: []any{shared, session, tableOn, hintOn, string(doc)}}
}

func tableRow(layout, readable, writable bool) fakeHintRow {
	return fakeHintRow{vals: []any{layout, readable, writable}}
}

func dbSetting(role string, inDatabase bool, config ...string) roleSetting {
	return roleSetting{Role: role, Database: inDatabase, Config: config}
}

const tableOnConf = "pg_hint_plan.enable_hint_table=on"
const tableOffConf = "pg_hint_plan.enable_hint_table=off"

func probeFake(t *testing.T, settings, table fakeHintRow) (*HintPlanAvailability, error) {
	t.Helper()
	db := &fakeHintDB{}
	db.set(settings, table)
	got, err := ProbeHintReadiness(context.Background(), db)
	if got == nil {
		t.Fatal("ProbeHintReadiness returned nil")
	}
	if got.CheckedAt.IsZero() {
		t.Error("a probed availability has no CheckedAt")
	}
	return got, err
}

// scopeCase is one settings layout and the facts it must give.
type scopeCase struct {
	name     string
	settings fakeHintRow
	effect   bool
	preload  bool
	tableOn  bool
	method   string
	reasons  []string
}

// tableSettingCases vary where enable_hint_table is set, with PostgreSQL's
// precedence, for a shared-preloaded module.
func tableSettingCases() []scopeCase {
	return []scopeCase{
		{"shared preload, enable_hint_table at its default off",
			settingsRow(sharedWithHints, "", "off"), false, true, false,
			"shared_preload", []string{reasonHintTableOff}},
		{"enable_hint_table unset reads as off",
			settingsRow(sharedWithHints, "", ""), false, true, false,
			"shared_preload", []string{reasonHintTableOff}},
		{"server-level on", settingsRow(sharedWithHints, "", "on"), true, true, true,
			"shared_preload", nil},
		{"ALTER DATABASE SET on",
			settingsRow(sharedWithHints, "", "off", dbSetting("", true, tableOnConf)),
			true, true, true, "shared_preload", nil},
		{"role-level setting in this database",
			settingsRow(sharedWithHints, "", "off", dbSetting("app", true, tableOnConf)),
			true, true, true, "shared_preload", nil},
		{"role-level setting for every database",
			settingsRow(sharedWithHints, "", "off", dbSetting("app", false, tableOnConf)),
			true, true, true, "shared_preload", nil},
		{"database setting beats ALTER ROLE ALL",
			settingsRow(sharedWithHints, "", "off", dbSetting("", false, tableOnConf),
				dbSetting("", true, tableOffConf)), false, true, false,
			"shared_preload", []string{reasonHintTableOff}},
		{"one role turning it off leaves the database default on",
			settingsRow(sharedWithHints, "", "off", dbSetting("", true, tableOnConf),
				dbSetting("batch", true, tableOffConf)), true, true, true,
			"shared_preload", nil},
		{"role setting in this database beats the role's global setting",
			settingsRow(sharedWithHints, "", "off", dbSetting("app", false, tableOnConf),
				dbSetting("app", true, tableOffConf)), false, true, false,
			"shared_preload", []string{reasonHintTableOff}},
	}
}

// preloadCases vary where pg_hint_plan is preloaded.
func preloadCases() []scopeCase {
	return []scopeCase{
		{"server session_preload_libraries",
			settingsRow("pg_stat_statements", "pg_hint_plan", "on"), true, true, true,
			"session_preload", nil},
		{"session_preload for the role that turns the table on",
			settingsRow("", "", "off",
				dbSetting("app", true, "session_preload_libraries=pg_hint_plan"),
				dbSetting("app", false, tableOnConf)), true, true, true,
			"session_preload", nil},
		{"session_preload only for another role",
			settingsRow("", "", "off",
				dbSetting("app", true, "session_preload_libraries=pg_hint_plan"),
				dbSetting("report", true, tableOnConf)), false, true, true,
			"session_preload", []string{reasonRoleMismatch}},
		{"a role's own session_preload list replaces the server list",
			settingsRow("", "pg_hint_plan", "off",
				dbSetting("app", true, "session_preload_libraries=auto_explain",
					tableOnConf)), false, true, true,
			"session_preload", []string{reasonRoleMismatch}},
		{"not preloaded anywhere", settingsRow("pg_stat_statements", "", "on"),
			false, false, true, "unavailable", []string{reasonNotPreloaded}},
		{"shared_preload_libraries unreadable", settingsRow(nil, "", "on"),
			false, false, true, "unavailable", []string{reasonPreloadUnreadable}},
		{"malformed setconfig entry is ignored",
			settingsRow(sharedWithHints, "", "off",
				dbSetting("", true, "pg_hint_plan.enable_hint_table")), false, true, false,
			"shared_preload", []string{reasonHintTableOff}},
	}
}

func TestProbeHintReadiness_Scopes(t *testing.T) {
	ready := tableRow(true, true, true)
	cases := append(tableSettingCases(), preloadCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := probeFake(t, tc.settings, ready)
			if err != nil {
				t.Fatalf("probe error: %v", err)
			}
			if got.ApplicationEffect != tc.effect || got.Preload != tc.preload ||
				got.HintTableOn != tc.tableOn || got.Method != tc.method {
				t.Fatalf("effect=%t preload=%t tableOn=%t method=%q, want %t %t %t %q",
					got.ApplicationEffect, got.Preload, got.HintTableOn, got.Method,
					tc.effect, tc.preload, tc.tableOn, tc.method)
			}
			if !reflect.DeepEqual(got.Reasons, tc.reasons) {
				t.Fatalf("reasons = %q, want %q", got.Reasons, tc.reasons)
			}
			if !got.HintTableReady || !got.Privilege {
				t.Fatalf("table facts lost: ready=%t privilege=%t",
					got.HintTableReady, got.Privilege)
			}
		})
	}
}

func TestProbeHintReadiness_TableFacts(t *testing.T) {
	on := settingsRow(sharedWithHints, "", "on")
	cases := []struct {
		name                       string
		table                      fakeHintRow
		ready, privilege, reasonOK bool
		reasons                    []string
	}{
		{"no table or a pre-1.7 layout", tableRow(false, false, false), false, false, false,
			[]string{reasonLayout}},
		{"readable without INSERT or DELETE", tableRow(true, true, false), true, false, false,
			[]string{reasonPrivilege}},
		{"layout without USAGE or SELECT", tableRow(true, false, true), false, true, false,
			[]string{reasonPrivilege}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := probeFake(t, on, tc.table)
			if err != nil {
				t.Fatalf("probe error: %v", err)
			}
			if got.ApplicationEffect || got.HintTableReady != tc.ready ||
				got.Privilege != tc.privilege {
				t.Fatalf("effect=%t ready=%t privilege=%t, want false %t %t",
					got.ApplicationEffect, got.HintTableReady, got.Privilege,
					tc.ready, tc.privilege)
			}
			if !reflect.DeepEqual(got.Reasons, tc.reasons) {
				t.Fatalf("reasons = %q, want %q", got.Reasons, tc.reasons)
			}
			if got.Readiness() != strings.Join(tc.reasons, "; ") {
				t.Fatalf("Readiness() = %q", got.Readiness())
			}
		})
	}
}

// Error propagation: a failed read is never ready; the error carries what
// was read, and the reason says the probe failed.
func TestProbeHintReadiness_ReadErrorsAreNotReady(t *testing.T) {
	boom := errors.New("connection reset by peer")
	ready := tableRow(true, true, true)
	on := settingsRow(sharedWithHints, "", "on")
	cases := []struct {
		name            string
		settings, table fakeHintRow
		wantErr         string
	}{
		{"pg_settings read fails", fakeHintRow{err: boom}, ready, "pg_hint_plan settings"},
		{"malformed pg_db_role_setting rows",
			fakeHintRow{vals: []any{sharedWithHints, "", "on", "on", "{not json"}}, ready,
			"pg_db_role_setting"},
		{"hint table read fails", on, fakeHintRow{err: boom}, "hint_plan.hints"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := probeFake(t, tc.settings, tc.table)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
			if got.ApplicationEffect {
				t.Fatal("a failed read reported application effect")
			}
			if !containsReason(got.Reasons, reasonProbeFailed) {
				t.Fatalf("reasons = %q, want %q", got.Reasons, reasonProbeFailed)
			}
		})
	}
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// Boundary: a LOAD into pg_sage's own backend loads the module for pg_sage
// alone, so it never gives application effect.
func TestWithSessionLoad_OnlyLoadIsNotApplicationEffect(t *testing.T) {
	got, err := probeFake(t, settingsRow("pg_stat_statements", "", "on"),
		tableRow(true, true, true))
	if err != nil {
		t.Fatal(err)
	}
	withSessionLoad(got, true)
	if !got.SessionLoad || got.Method != "session_load" {
		t.Fatalf("session load = %t method %q, want true session_load",
			got.SessionLoad, got.Method)
	}
	if got.ApplicationEffect || !reflect.DeepEqual(got.Reasons, []string{reasonNotPreloaded}) {
		t.Fatalf("LOAD-only: effect=%t reasons=%q", got.ApplicationEffect, got.Reasons)
	}
	withSessionLoad(got, false)
	if got.SessionLoad || got.Method != "unavailable" {
		t.Fatalf("no load: session=%t method=%q", got.SessionLoad, got.Method)
	}
}

// The probe never changes application-visible state and is bounded.
func TestProbeHintReadiness_ReadOnlyAndBounded(t *testing.T) {
	db := &fakeHintDB{}
	db.set(settingsRow(sharedWithHints, "", "on"), tableRow(true, true, true))
	if _, err := ProbeHintReadiness(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if len(db.sqls) != 2 {
		t.Fatalf("probe ran %d statements, want the two catalog reads: %q",
			len(db.sqls), db.sqls)
	}
	for i, sql := range db.sqls {
		upper := strings.ToUpper(sql)
		if !strings.HasPrefix(sql, "/* pg_sage tuner */") ||
			!strings.Contains(upper, "SELECT") {
			t.Errorf("statement %d is not a tagged read: %q", i, sql)
		}
		for _, verb := range []string{"SET ", "ALTER ", "LOAD ", "INSERT ", "UPDATE ",
			"DELETE "} {
			if strings.Contains(upper, "\n"+verb) || strings.HasPrefix(upper, verb) {
				t.Errorf("statement %d runs %s: %q", i, verb, sql)
			}
		}
		if left := db.deadlines[i]; left <= 0 || left > hintProbeTimeout {
			t.Errorf("statement %d deadline %v, want within %v", i, left, hintProbeTimeout)
		}
	}
}

// Nil/empty: without a connection nothing is read and nothing is ready.
func TestProbeHintReadiness_NilQuerier(t *testing.T) {
	var typedNil *pgxpool.Pool
	for name, q := range map[string]HintQuerier{"nil": nil, "typed nil pool": typedNil} {
		got, err := ProbeHintReadiness(context.Background(), q)
		if err != nil || got == nil {
			t.Fatalf("%s: got %+v, %v", name, got, err)
		}
		if got.ApplicationEffect || got.Preload || got.Method != "unavailable" ||
			!reflect.DeepEqual(got.Reasons, []string{reasonNoConnection}) ||
			!got.CheckedAt.IsZero() {
			t.Fatalf("%s: %+v, want the unavailable struct", name, got)
		}
	}
	got, err := DetectHintPlan(context.Background(), nil)
	if err != nil || got == nil || got.ApplicationEffect || got.Method != "unavailable" ||
		!reflect.DeepEqual(got.Reasons, []string{reasonNoConnection}) {
		t.Fatalf("DetectHintPlan(nil) = %+v, %v", got, err)
	}
}

func TestHasHintLibrary(t *testing.T) {
	cases := map[string]bool{
		"pg_hint_plan":                         true,
		" pg_stat_statements , pg_hint_plan ":  true,
		`"pg_hint_plan"`:                       true,
		`'$libdir/pg_hint_plan.so'`:            true,
		`$libdir/plugins/pg_hint_plan`:         true,
		"pg_hint_plan.dll":                     true,
		"":                                     false,
		"pg_hint_planner":                      false,
		"my_pg_hint_plan":                      false,
		"pg_stat_statements,auto_explain":      false,
		"pg_hint_plan_extra, pg_stat_monitor ": false,
	}
	for in, want := range cases {
		if got := hasHintLibrary(in); got != want {
			t.Errorf("hasHintLibrary(%q) = %t, want %t", in, got, want)
		}
	}
}

func TestSettingIsOn(t *testing.T) {
	for _, v := range []string{"on", "ON", " true ", "yes", "1", "t", "y", "'on'"} {
		if !settingIsOn(v) {
			t.Errorf("settingIsOn(%q) = false", v)
		}
	}
	for _, v := range []string{"off", "false", "0", "", "o", "onn", "no", "maybe"} {
		if settingIsOn(v) {
			t.Errorf("settingIsOn(%q) = true", v)
		}
	}
}

// pg_hint_plan.enable_hint is the module's master switch: with it off no
// hint applies, hint table rows included, whatever enable_hint_table says.
func TestProbeHintReadiness_EnableHintOff(t *testing.T) {
	ready := tableRow(true, true, true)
	hintOff := "pg_hint_plan.enable_hint=off"
	cases := []struct {
		name     string
		settings fakeHintRow
		effect   bool
		reasons  []string
	}{
		{"server enable_hint off", settingsRowHint(sharedWithHints, "", "on", "off"),
			false, []string{reasonHintsOff}},
		{"server value hidden by pg_sage's own session reads as off",
			settingsRowHint(sharedWithHints, "", "on", ""), false, []string{reasonHintsOff}},
		{"ALTER DATABASE SET enable_hint off",
			settingsRow(sharedWithHints, "", "on", dbSetting("", true, hintOff)),
			false, []string{reasonHintsOff}},
		{"enable_hint off for the only role with the table on",
			settingsRow(sharedWithHints, "", "off",
				dbSetting("app", true, tableOnConf, hintOff)),
			false, []string{reasonHintsOff}},
		{"database off, a role turns both back on",
			settingsRowHint(sharedWithHints, "", "off", "on", dbSetting("", true, hintOff),
				dbSetting("app", true, tableOnConf, "pg_hint_plan.enable_hint=on")),
			true, nil},
		{"enable_hint_table off is reported first",
			settingsRowHint(sharedWithHints, "", "off", "off"),
			false, []string{reasonHintTableOff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := probeFake(t, tc.settings, ready)
			if err != nil {
				t.Fatalf("probe error: %v", err)
			}
			if got.ApplicationEffect != tc.effect || got.HintTableOn != tc.effect {
				t.Fatalf("effect=%t tableOn=%t, want %t", got.ApplicationEffect,
					got.HintTableOn, tc.effect)
			}
			if !reflect.DeepEqual(got.Reasons, tc.reasons) {
				t.Fatalf("reasons = %q, want %q", got.Reasons, tc.reasons)
			}
		})
	}
}
