-- AP-01: a registered agent role (the Guard agent role naming: a fixed
-- prefix and 10 base32 characters) that bypasses row-level security.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='sage_agentb_sbattrbypa') THEN
    CREATE ROLE sage_agentb_sbattrbypa NOLOGIN;
  END IF;
END $$;
ALTER ROLE sage_agentb_sbattrbypa BYPASSRLS;
