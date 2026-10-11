-- AP-02: a registered agent role that owns a table (its memory store).
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='sage_agentb_sbownstore') THEN
    CREATE ROLE sage_agentb_sbownstore NOLOGIN;
  END IF;
END $$;
DROP SCHEMA IF EXISTS sb_ps_agentown CASCADE;
CREATE SCHEMA sb_ps_agentown;
CREATE TABLE sb_ps_agentown.memory (id bigint PRIMARY KEY, note text);
ALTER TABLE sb_ps_agentown.memory OWNER TO sage_agentb_sbownstore;
