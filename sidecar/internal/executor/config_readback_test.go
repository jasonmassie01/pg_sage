package executor

import "testing"

// No concurrent access tests: settingMatches is a pure comparison.

func TestSettingMatches(t *testing.T) {
	kb := settingRow{Setting: "65536", Unit: "kB", VarType: "integer"}
	pages := settingRow{Setting: "128", Unit: "8kB", VarType: "integer"}
	secs := settingRow{Setting: "300", Unit: "s", VarType: "integer"}
	ratio := settingRow{Setting: "1.1", VarType: "real"}
	delay := settingRow{Setting: "2", Unit: "ms", VarType: "real"}
	on := settingRow{Setting: "on", VarType: "bool"}
	enum := settingRow{Setting: "try", VarType: "enum"}
	cases := []struct {
		row  settingRow
		want string
		ok   bool
	}{
		{kb, "64MB", true},
		{kb, "'64MB'", true},
		{kb, "65536", true},
		{kb, "65536kB", true},
		{kb, "32MB", false},
		{kb, "64mb", false}, // PostgreSQL rejects the unit; never "in effect"
		{pages, "1MB", true},
		{pages, "128", true},
		{pages, "2MB", false},
		{secs, "5min", true},
		{secs, "300", true},
		{secs, "300s", true},
		{secs, "6min", false},
		{ratio, "1.1", true},
		{ratio, "1.10", true},
		{ratio, "1.2", false},
		{delay, "2ms", true},
		{delay, "2", true},
		{delay, "3ms", false},
		{on, "true", true},
		{on, "ON", true},
		{on, "yes", true},
		{on, "1", true},
		{on, "off", false},
		{enum, "TRY", true},
		{enum, "on", false},
		{kb, "", false},
		{settingRow{Setting: "abc", Unit: "kB", VarType: "integer"}, "64MB", false},
	}
	for _, c := range cases {
		if got := settingMatches(c.row, c.want); got != c.ok {
			t.Errorf("settingMatches(%+v, %q) = %v, want %v", c.row, c.want, got, c.ok)
		}
	}
}

// A RESET is confirmed when the setting no longer comes from
// postgresql.auto.conf; without sourcefile visibility it cannot be told.
func TestResetConfirmed(t *testing.T) {
	cases := []struct {
		row       settingRow
		confirmed bool
		known     bool
	}{
		{settingRow{Source: "default"}, true, true},
		{settingRow{Source: "override"}, true, true},
		{settingRow{Source: "configuration file",
			SourceFile: "/data/postgresql.conf"}, true, true},
		{settingRow{Source: "configuration file",
			SourceFile: "/data/postgresql.auto.conf"}, false, true},
		{settingRow{Source: "configuration file"}, false, false},
		{settingRow{Source: "command line"}, true, true},
	}
	for _, c := range cases {
		confirmed, known := resetConfirmed(c.row)
		if confirmed != c.confirmed || known != c.known {
			t.Errorf("resetConfirmed(%+v) = %v,%v want %v,%v", c.row, confirmed, known,
				c.confirmed, c.known)
		}
	}
}
