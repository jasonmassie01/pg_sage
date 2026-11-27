package tuner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Reasons a hint written to hint_plan.hints would not change an
// application's plan (tuning-04). The finding detail hint_readiness joins
// them with "; ".
const (
	reasonNotPreloaded      = "not preloaded"
	reasonPreloadUnreadable = "shared_preload_libraries unreadable"
	reasonHintTableOff      = "enable_hint_table off"
	reasonHintsOff          = "enable_hint off"
	reasonRoleMismatch      = "preload and enable_hint_table differ by role"
	reasonLayout            = "hint table layout"
	reasonPrivilege         = "privilege"
	reasonProbeFailed       = "probe failed"
	reasonNoConnection      = "no connection"
	reasonNotDetected       = "pg_hint_plan not detected"
)

// hintProbeTimeout bounds the readiness probe's two catalog reads.
const hintProbeTimeout = 5 * time.Second

// ProbeHintReadiness reads, from the catalog only, the four facts that
// decide whether hints in hint_plan.hints change application plans:
// pg_hint_plan is preloaded for application sessions, enable_hint_table is
// on for them (server, ALTER DATABASE and ALTER ROLE settings), the table
// has the 1.7 layout with its unique index, and pg_sage may write it. It
// never runs SET, ALTER or LOAD. The result is never nil; a failed read
// leaves its facts false, adds reasonProbeFailed and returns the error.
func ProbeHintReadiness(ctx context.Context, q HintQuerier) (*HintPlanAvailability, error) {
	if p, ok := q.(*pgxpool.Pool); q == nil || (ok && p == nil) {
		return unavailableHintPlan(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, hintProbeTimeout)
	defer cancel()
	out := &HintPlanAvailability{CheckedAt: time.Now()}
	var errs []error
	sameScope := false
	if set, err := readHintSettings(ctx, q); err != nil {
		errs = append(errs, err)
	} else {
		sameScope = set.apply(out)
	}
	layout, readable, writable, err := readHintTable(ctx, q)
	if err != nil {
		errs = append(errs, err)
	}
	out.HintTableReady = layout && readable
	out.Privilege = writable
	out.ApplicationEffect = sameScope && out.HintTableReady && out.Privilege
	out.Method = hintMethod(out)
	if len(errs) > 0 {
		out.Reasons = append(out.Reasons, reasonProbeFailed)
		return out, fmt.Errorf("probe pg_hint_plan readiness: %w", errors.Join(errs...))
	}
	if !layout {
		out.Reasons = append(out.Reasons, reasonLayout)
	} else if !readable || !writable {
		out.Reasons = append(out.Reasons, reasonPrivilege)
	}
	return out, nil
}

// hintSettings are the settings that decide which sessions load
// pg_hint_plan and read its table.
type hintSettings struct {
	shared  *string // shared_preload_libraries; nil when not readable
	session string  // server-level session_preload_libraries
	tableOn string  // server-level pg_hint_plan.enable_hint_table
	hintOn  string  // server-level pg_hint_plan.enable_hint ("" when unknown)
	roles   []roleSetting
}

// roleSetting is one pg_db_role_setting row for this database or for all
// databases. Role is empty for a row that applies to every role.
type roleSetting struct {
	Role     string   `json:"role"`
	Database bool     `json:"database"`
	Config   []string `json:"config"`
}

// readHintSettings reads the settings in one statement (hintSettingsSQL).
func readHintSettings(ctx context.Context, q HintQuerier) (hintSettings, error) {
	var s hintSettings
	var roles string
	err := q.QueryRow(ctx, hintSettingsSQL).Scan(&s.shared, &s.session, &s.tableOn,
		&s.hintOn, &roles)
	if err != nil {
		return s, fmt.Errorf("read pg_hint_plan settings: %w", err)
	}
	if err := json.Unmarshal([]byte(roles), &s.roles); err != nil {
		return s, fmt.Errorf("decode pg_db_role_setting rows: %w", err)
	}
	return s, nil
}

// hintSettingsSQL reads shared_preload_libraries (NULL when this role may
// not see it); the server-level session_preload_libraries,
// enable_hint_table and enable_hint (the module's master switch: on, its
// default, when the module is not loaded here, and empty when only pg_sage's
// own database, role or session value shows); and the ALTER DATABASE / ALTER
// ROLE settings for this database and for all databases (login roles only: a
// setting on a group role is not applied to its members). pg_settings shows
// pg_sage's own session, so a value whose source is pg_sage's database, role
// or session is not taken as the server's; those settings come from
// pg_db_role_setting.
const hintSettingsSQL = `/* pg_sage tuner */
SELECT
  (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'shared_preload_libraries'),
  coalesce((SELECT setting FROM pg_catalog.pg_settings
             WHERE name = 'session_preload_libraries'
               AND source IN ('default', 'configuration file', 'command line',
                              'environment variable')), ''),
  coalesce((SELECT setting FROM pg_catalog.pg_settings
             WHERE name = 'pg_hint_plan.enable_hint_table'
               AND source IN ('default', 'configuration file', 'command line',
                              'environment variable')), ''),
  coalesce((SELECT CASE WHEN source IN ('default', 'configuration file', 'command line',
                                        'environment variable') THEN setting ELSE '' END
              FROM pg_catalog.pg_settings
             WHERE name = 'pg_hint_plan.enable_hint'), 'on'),
  coalesce((SELECT json_agg(json_build_object(
                     'role', coalesce(r.rolname, ''),
                     'database', s.setdatabase <> 0,
                     'config', s.setconfig))
              FROM pg_catalog.pg_db_role_setting s
              LEFT JOIN pg_catalog.pg_roles r ON r.oid = s.setrole
             WHERE s.setdatabase IN (0, (SELECT oid FROM pg_catalog.pg_database
                                          WHERE datname = current_database()))
               AND (s.setrole = 0 OR r.rolcanlogin)), '[]')::text`

// Readiness names the missing prerequisites ("" when hints take effect).
func (a *HintPlanAvailability) Readiness() string {
	if a != nil && a.ApplicationEffect {
		return ""
	}
	if a == nil || len(a.Reasons) == 0 {
		return reasonNotDetected
	}
	return strings.Join(a.Reasons, "; ")
}

// hasHintLibrary reports whether a preload list names pg_hint_plan, with
// or without quotes, a directory or a shared-library suffix.
func hasHintLibrary(list string) bool {
	for _, lib := range strings.Split(list, ",") {
		lib = strings.Trim(strings.TrimSpace(lib), `"'`)
		lib = path.Base(strings.ReplaceAll(lib, `\`, "/"))
		for _, ext := range []string{".so", ".dll", ".dylib"} {
			lib = strings.TrimSuffix(lib, ext)
		}
		if lib == "pg_hint_plan" {
			return true
		}
	}
	return false
}

// settingIsOn reads a PostgreSQL boolean setting value.
func settingIsOn(v string) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `'"`)) {
	case "on", "true", "yes", "1", "t", "y":
		return true
	}
	return false
}
