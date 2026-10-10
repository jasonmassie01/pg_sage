-- Agent roles are cluster-wide: drop the scenario's so no later posture run
-- counts it as a registered agent.
DROP ROLE IF EXISTS sage_agentb_sbattrbypa;
