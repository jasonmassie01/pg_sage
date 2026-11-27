-- pg_sage CI test server (ci-03b): the suites read pg_stat_statements in
-- the postgres database from their first query.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
