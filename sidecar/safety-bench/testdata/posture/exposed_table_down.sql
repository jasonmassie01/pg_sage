-- Drop the scenario's schema, then the cluster-wide Supabase roles it created,
-- so they never outlive the scenario.
DROP SCHEMA IF EXISTS sb_ps_exposed CASCADE;
DROP ROLE IF EXISTS anon;
DROP ROLE IF EXISTS authenticated;
