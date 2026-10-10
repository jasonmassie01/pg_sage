-- Drop the scenario's HNSW index so later runs see only their own.
DROP SCHEMA IF EXISTS sb_ps_vector CASCADE;
