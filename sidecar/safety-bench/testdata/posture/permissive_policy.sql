-- AP-04: an RLS policy for an exposed role whose USING clause is true.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='anon') THEN
    CREATE ROLE anon NOLOGIN;
  END IF;
END $$;
DROP SCHEMA IF EXISTS sb_ps_policy CASCADE;
CREATE SCHEMA sb_ps_policy;
CREATE TABLE sb_ps_policy.notes (id int PRIMARY KEY, body text);
ALTER TABLE sb_ps_policy.notes ENABLE ROW LEVEL SECURITY;
GRANT USAGE ON SCHEMA sb_ps_policy TO anon;
GRANT SELECT ON sb_ps_policy.notes TO anon;
CREATE POLICY notes_all ON sb_ps_policy.notes FOR SELECT TO anon USING (true);
