package schema

// evid-03a: sage.facts takes the owner brief (fact type owner_brief about
// subject kind database, subject "*") and the do-not-touch fact of
// evid-03b. The single-column CHECKs on fact_type and subject_kind are
// found in pg_constraint, whatever an earlier step named them (the inline
// *_check of ddlFacts or the *_v2 of ddlAgentEnvClass), and replaced once
// by *_v3 constraints that keep every earlier value. Rows written before
// keep their values, which the v3 lists include. Idempotent: a second boot
// finds the v3 constraints and changes nothing.
const ddlFactsOwnerBrief = `
SET LOCAL lock_timeout = '5s';
DO $$
DECLARE
  col text;
  con record;
BEGIN
  FOREACH col IN ARRAY ARRAY['fact_type', 'subject_kind'] LOOP
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
               WHERE conrelid = 'sage.facts'::regclass
                 AND conname = 'facts_' || col || '_v3') THEN
      CONTINUE;
    END IF;
    FOR con IN SELECT c.conname FROM pg_catalog.pg_constraint c
                 JOIN pg_catalog.pg_attribute a ON a.attrelid = c.conrelid
                WHERE c.conrelid = 'sage.facts'::regclass AND c.contype = 'c'
                  AND a.attname = col AND c.conkey = ARRAY[a.attnum] LOOP
      EXECUTE format('ALTER TABLE sage.facts DROP CONSTRAINT %I', con.conname);
    END LOOP;
    IF col = 'fact_type' THEN
      ALTER TABLE sage.facts ADD CONSTRAINT facts_fact_type_v3 CHECK (fact_type IN (
          'owned_by_app_migrations', 'test_fixture', 'slot_consumer', 'append_only',
          'table_window', 'column_class', 'owner_brief', 'do_not_touch'));
    ELSE
      ALTER TABLE sage.facts ADD CONSTRAINT facts_subject_kind_v3 CHECK (subject_kind IN (
          'index', 'table', 'schema', 'slot', 'column', 'database'));
    END IF;
  END LOOP;
END $$;
`
