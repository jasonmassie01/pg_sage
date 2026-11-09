package rolegrants

// QueryTextVisibleSQL is a boolean expression, true when the current role
// sees every role's query text in pg_stat_statements. It makes the test
// pg_stat_statements itself makes: on PostgreSQL 15+ the privileges of
// pg_read_all_stats (INHERIT membership; a NOINHERIT member only holds the
// role), on PostgreSQL 14 membership alone. A superuser passes both. Other
// roles' rows read '<insufficient privilege>', not NULL, so the rows cannot
// answer it. The startup checks and the first look both use it, so they
// agree.
const QueryTextVisibleSQL = `pg_catalog.pg_has_role(current_user, 'pg_read_all_stats',
    CASE WHEN pg_catalog.current_setting('server_version_num')::int >= 150000
         THEN 'USAGE' ELSE 'MEMBER' END)`
