package pgconf

import (
	"reflect"
	"testing"
)

// No concurrent access tests: the parsers are pure functions over their
// argument and share no state.

func TestParseAlterSystem(t *testing.T) {
	cases := []struct {
		sql  string
		want SystemStmt
		ok   bool
	}{
		{"ALTER SYSTEM SET work_mem = '16MB';", SystemStmt{Name: "work_mem", Value: "16MB"}, true},
		{"alter system set random_page_cost to 1.1", SystemStmt{
			Name: "random_page_cost", Value: "1.1"}, true},
		{"ALTER SYSTEM SET Work_Mem='64MB'", SystemStmt{Name: "work_mem", Value: "64MB"}, true},
		{"  ALTER SYSTEM SET \"work_mem\" = 4096 ;  ", SystemStmt{
			Name: "work_mem", Value: "4096"}, true},
		{"ALTER SYSTEM SET log_line_prefix = 'it''s %m'", SystemStmt{
			Name: "log_line_prefix", Value: "it's %m"}, true},
		{"ALTER SYSTEM RESET shared_buffers;", SystemStmt{
			Name: "shared_buffers", Reset: true}, true},
		{"ALTER SYSTEM SET autovacuum_max_workers TO 5", SystemStmt{
			Name: "autovacuum_max_workers", Value: "5"}, true},
		// RESET ALL is not a single-setting change.
		{"ALTER SYSTEM RESET ALL", SystemStmt{}, false},
		{"ALTER SYSTEM SET work_mem =", SystemStmt{}, false},
		{"ALTER SYSTEM SET work_mem", SystemStmt{}, false},
		{"ALTER SYSTEM SET = '4MB'", SystemStmt{}, false},
		{"ALTER DATABASE app SET work_mem = '4MB'", SystemStmt{}, false},
		{"CREATE INDEX ON t (a)", SystemStmt{}, false},
		{"", SystemStmt{}, false},
	}
	for _, c := range cases {
		got, ok := ParseAlterSystem(c.sql)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseAlterSystem(%q) = %+v,%v want %+v,%v", c.sql, got, ok, c.want, c.ok)
		}
	}
}

func TestParseAlterTableReloptions(t *testing.T) {
	cases := []struct {
		sql  string
		want TableStmt
		ok   bool
	}{
		{`ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.02);`,
			TableStmt{Table: `"public"."orders"`, Options: []Reloption{
				{Key: "autovacuum_vacuum_scale_factor", Value: "0.02"}}}, true},
		{`alter table public.orders set (Fillfactor=90, autovacuum_vacuum_threshold = 500)`,
			TableStmt{Table: "public.orders", Options: []Reloption{
				{Key: "fillfactor", Value: "90"},
				{Key: "autovacuum_vacuum_threshold", Value: "500"}}}, true},
		{`ALTER TABLE IF EXISTS ONLY public.t SET (toast.autovacuum_enabled = 'false')`,
			TableStmt{Table: "public.t", Options: []Reloption{
				{Key: "toast.autovacuum_enabled", Value: "false"}}}, true},
		{`ALTER TABLE public.t SET ("autovacuum_enabled")`,
			TableStmt{Table: "public.t", Options: []Reloption{
				{Key: "autovacuum_enabled", Value: "true"}}}, true},
		{`ALTER TABLE "my schema"."T" RESET (fillfactor, toast.autovacuum_vacuum_threshold);`,
			TableStmt{Table: `"my schema"."T"`, Reset: true, Options: []Reloption{
				{Key: "fillfactor"}, {Key: "toast.autovacuum_vacuum_threshold"}}}, true},
		// A comma inside a quoted value does not split the option list.
		{`ALTER TABLE t SET (x = 'a,b', y = 1)`,
			TableStmt{Table: "t", Options: []Reloption{
				{Key: "x", Value: "a,b"}, {Key: "y", Value: "1"}}}, true},
		{`ALTER TABLE t SET ()`, TableStmt{}, false},
		{`ALTER TABLE t SET (fillfactor = 90`, TableStmt{}, false},
		{`ALTER TABLE t ALTER COLUMN a SET NOT NULL`, TableStmt{}, false},
		{`ALTER TABLE t SET (fillfactor = 90), RESET (autovacuum_enabled)`, TableStmt{}, false},
		{`ALTER TABLE t SET TABLESPACE fast`, TableStmt{}, false},
		{`ALTER SYSTEM SET work_mem = '4MB'`, TableStmt{}, false},
		{``, TableStmt{}, false},
	}
	for _, c := range cases {
		got, ok := ParseAlterTableReloptions(c.sql)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseAlterTableReloptions(%q) = %+v,%v want %+v,%v",
				c.sql, got, ok, c.want, c.ok)
		}
	}
}

func TestReloptionBaseKey(t *testing.T) {
	cases := map[string]struct {
		base  string
		toast bool
	}{
		"toast.autovacuum_enabled": {"autovacuum_enabled", true},
		"fillfactor":               {"fillfactor", false},
		"TOAST.Fillfactor":         {"fillfactor", true},
	}
	for in, want := range cases {
		base, toast := ReloptionBaseKey(in)
		if base != want.base || toast != want.toast {
			t.Errorf("ReloptionBaseKey(%q) = %q,%v want %q,%v", in, base, toast,
				want.base, want.toast)
		}
	}
}
