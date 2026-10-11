-- Drop what the agent role owns, then the cluster-wide role itself.
DROP SCHEMA IF EXISTS sb_ps_agentown CASCADE;
DROP ROLE IF EXISTS sage_agentb_sbownstore;
