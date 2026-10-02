package executor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// settingRow is one pg_settings row as this session sees it.
type settingRow struct {
	Setting        string `json:"setting"`
	Unit           string `json:"unit,omitempty"`
	VarType        string `json:"vartype"`
	Context        string `json:"context"`
	Source         string `json:"source"`
	SourceFile     string `json:"sourcefile,omitempty"`
	PendingRestart bool   `json:"pending_restart"`
}

// readSettingRow reads a setting's effective value and provenance.
// sourcefile is visible only to superusers and pg_read_all_settings.
func readSettingRow(ctx context.Context, pool *pgxpool.Pool, name string) (settingRow, error) {
	var r settingRow
	err := pool.QueryRow(ctx, `/* pg_sage */ SELECT setting, COALESCE(unit, ''),
		vartype, context, source, COALESCE(sourcefile, ''), pending_restart
		FROM pg_settings WHERE name = $1`, strings.ToLower(name)).Scan(
		&r.Setting, &r.Unit, &r.VarType, &r.Context, &r.Source, &r.SourceFile,
		&r.PendingRestart)
	if err != nil {
		return settingRow{}, fmt.Errorf("read pg_settings for %s: %w", name, err)
	}
	return r, nil
}

// settingMatches reports whether the effective setting equals the wanted
// value, comparing in the setting's own unit and type.
func settingMatches(row settingRow, want string) bool {
	w := strings.TrimSpace(pgconf.Unquote(want))
	if w == "" {
		return false
	}
	switch row.VarType {
	case "bool":
		wb, ok1 := pgBool(w)
		rb, ok2 := pgBool(row.Setting)
		return ok1 && ok2 && wb == rb
	case "integer", "real":
		return numericSettingMatches(row, w)
	default:
		return strings.EqualFold(w, row.Setting)
	}
}

func numericSettingMatches(row settingRow, want string) bool {
	w, err := pgconf.ToBaseUnits(want, row.Unit)
	if err != nil {
		return false
	}
	s, err := strconv.ParseFloat(row.Setting, 64)
	if err != nil {
		return false
	}
	if row.VarType == "integer" {
		return math.Round(w) == s // PostgreSQL rounds to the base unit
	}
	return math.Abs(w-s) <= 1e-9*math.Max(1, math.Abs(s))
}

// pgBool parses a PostgreSQL boolean (unique prefixes of true/false/yes/
// no, on/off, 1/0).
func pgBool(v string) (bool, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	switch {
	case s == "on" || s == "1":
		return true, true
	case s == "off" || s == "0":
		return false, true
	case s == "":
		return false, false
	case strings.HasPrefix("true", s) || strings.HasPrefix("yes", s):
		return true, true
	case strings.HasPrefix("false", s) || strings.HasPrefix("no", s):
		return false, true
	}
	return false, false
}

// resetConfirmed reports whether an ALTER SYSTEM RESET is live: the
// setting no longer comes from postgresql.auto.conf. known is false when
// the file cannot be attributed (sourcefile hidden from this role).
func resetConfirmed(row settingRow) (confirmed, known bool) {
	if row.Source != "configuration file" {
		return true, true
	}
	if row.SourceFile == "" {
		return false, false
	}
	return !isAutoConf(row.SourceFile), true
}

func isAutoConf(path string) bool {
	return strings.HasSuffix(path, "postgresql.auto.conf")
}

// readbackState classifies a read-back after ALTER SYSTEM + reload.
type readbackState string

const (
	readbackInEffect       readbackState = "in_effect"
	readbackPendingRestart readbackState = "pending_restart"
	readbackNotInEffect    readbackState = "not_in_effect"
	readbackUnconfirmed    readbackState = "unconfirmed"
)

// awaitReadback polls pg_settings until the statement's value is live, a
// restart is pending, or wait passes. pg_reload_conf only signals the
// postmaster; each backend applies the file when its own SIGHUP arrives.
func awaitReadback(ctx context.Context, pool *pgxpool.Pool, stmt pgconf.SystemStmt,
	wait time.Duration) (settingRow, readbackState, error) {
	deadline := time.Now().Add(wait)
	for {
		row, err := readSettingRow(ctx, pool, stmt.Name)
		if err != nil {
			return row, readbackUnconfirmed, err
		}
		state, done := classifyReadback(row, stmt)
		if done || time.Now().After(deadline) {
			return row, state, nil
		}
		select {
		case <-ctx.Done():
			return row, readbackUnconfirmed, ctx.Err()
		case <-time.After(reloadSettleInterval):
		}
	}
}

// classifyReadback returns the current state and whether it is final.
func classifyReadback(row settingRow, stmt pgconf.SystemStmt) (readbackState, bool) {
	if stmt.Reset {
		confirmed, known := resetConfirmed(row)
		switch {
		case confirmed && !row.PendingRestart:
			return readbackInEffect, true
		case confirmed:
			return readbackPendingRestart, false // the reload may still clear it
		case !known:
			return readbackUnconfirmed, false
		}
		return readbackNotInEffect, false
	}
	if settingMatches(row, stmt.Value) {
		return readbackInEffect, true
	}
	if row.Context == "postmaster" && row.PendingRestart {
		return readbackPendingRestart, true
	}
	return readbackNotInEffect, false
}
