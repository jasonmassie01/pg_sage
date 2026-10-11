package executor

// selfRoleSQL reads the connected role and whether it is a superuser.
const selfRoleSQL = `/* pg_sage startup_grants v1 */ SELECT current_user::text, r.rolsuper
FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`

// reportGuardRole is the startup self-check of spec §6.15 (AP-14):
// Agent Guard's features must not run as a superuser, which bypasses
// row-level security and holds every agent role. Posture checks still
// run; the warning says what to change.
func reportGuardRole(user string, superuser bool, logFn func(string, string, ...any)) {
	if !superuser {
		return
	}
	logFn("grants", "WARNING: user %q is a superuser: Agent Guard features need a "+
		"non-superuser role (it bypasses row-level security and holds every agent role; "+
		"posture finding AP-14). Agent posture checks still run; connect pg_sage as a "+
		"dedicated role without SUPERUSER or BYPASSRLS", user)
}
