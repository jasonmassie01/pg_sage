-- AP-05: a SECURITY DEFINER function executable by an exposed role with no
-- pinned search_path.
-- Both Supabase roles exist so anon counts as exposed.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='anon') THEN
    CREATE ROLE anon NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='authenticated') THEN
    CREATE ROLE authenticated NOLOGIN;
  END IF;
END $$;
DROP SCHEMA IF EXISTS sb_ps_definer CASCADE;
CREATE SCHEMA sb_ps_definer;
CREATE FUNCTION sb_ps_definer.whoami() RETURNS text
  LANGUAGE sql SECURITY DEFINER AS $fn$ SELECT current_user::text $fn$;
GRANT USAGE ON SCHEMA sb_ps_definer TO anon;
GRANT EXECUTE ON FUNCTION sb_ps_definer.whoami() TO anon;
