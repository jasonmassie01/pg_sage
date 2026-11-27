package collector

import (
	"context"
	"fmt"
)

// markSetPrivileges sets SetPrivilege on each superuser-context row the
// connected role may SET at database scope: every one for a superuser,
// and on PostgreSQL 15+ those has_parameter_privilege grants. PostgreSQL
// 14 has no parameter privileges, so a non-superuser gets none. It reads
// the role once per snapshot and skips the server when no row needs it.
// On error every row stays unprivileged (fail closed).
func markSetPrivileges(ctx context.Context, pool catalogQuerier, settings []PGSetting) error {
	var names []string
	for _, s := range settings {
		if s.Context == "superuser" {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	var super bool
	var version int
	if err := pool.QueryRow(ctx, `/* pg_sage collector */ SELECT r.rolsuper,
		current_setting('server_version_num')::int
		FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`).
		Scan(&super, &version); err != nil {
		return fmt.Errorf("SET privilege: read the connected role: %w", err)
	}
	granted := make(map[string]bool, len(names))
	switch {
	case super:
		for _, name := range names {
			granted[name] = true
		}
	case version >= 150000:
		if err := grantedParameters(ctx, pool, names, granted); err != nil {
			return err
		}
	}
	for i := range settings {
		settings[i].SetPrivilege = settings[i].Context == "superuser" &&
			granted[settings[i].Name]
	}
	return nil
}

func grantedParameters(ctx context.Context, pool catalogQuerier, names []string,
	granted map[string]bool) error {
	rows, err := pool.Query(ctx, `/* pg_sage collector */ SELECT n FROM unnest($1::text[]) AS n
		WHERE pg_catalog.has_parameter_privilege(n, 'SET')`, names)
	if err != nil {
		return fmt.Errorf("SET privilege: has_parameter_privilege: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("SET privilege: scan parameter: %w", err)
		}
		granted[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("SET privilege: read parameters: %w", err)
	}
	return nil
}
