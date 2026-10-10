-- AP-03: a table granted to an exposed role with RLS left disabled. anon is
-- exposed when both Supabase roles exist.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='anon') THEN
    CREATE ROLE anon NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='authenticated') THEN
    CREATE ROLE authenticated NOLOGIN;
  END IF;
END $$;
DROP SCHEMA IF EXISTS sb_ps_exposed CASCADE;
CREATE SCHEMA sb_ps_exposed;
CREATE TABLE sb_ps_exposed.profiles (id int PRIMARY KEY, email text);
GRANT USAGE ON SCHEMA sb_ps_exposed TO anon;
GRANT SELECT ON sb_ps_exposed.profiles TO anon;
