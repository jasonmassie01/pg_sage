// Package backendguard holds the SQL predicates that keep every
// pg_cancel_backend / pg_terminate_backend pg_sage runs or proposes away
// from sessions it must never signal. The executor's approved signal SQL
// and the Cases playbook's guarded scripts both embed these constants, so
// the two cannot drift apart.
//
// The predicates expect pg_stat_activity aliased as "a" and, for
// TerminatePredicate, pg_roles joined as "r" through TerminateJoin.
package backendguard

// ProtectedPredicate excludes other databases, non-client backends
// (walsenders, autovacuum, background workers), backup/dump tools and
// pg_sage itself.
const ProtectedPredicate = `
   AND a.datname = current_database()
   AND a.backend_type = 'client backend'
   AND a.application_name NOT ILIKE '%pg_sage%'
   AND a.application_name NOT ILIKE '%pg_dump%'
   AND a.application_name NOT ILIKE '%pg_basebackup%'
   AND a.application_name NOT ILIKE '%pg_restore%'`

// TerminateJoin joins the session's role for TerminatePredicate.
const TerminateJoin = `
  JOIN pg_roles AS r ON r.rolname = a.usename`

// TerminatePredicate adds the terminate-only guards: never a superuser
// session and never anything that names itself autovacuum.
const TerminatePredicate = `
   AND NOT r.rolsuper
   AND a.application_name NOT ILIKE '%autovacuum%'`
