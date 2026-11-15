package schema

// evid-03a: sage.facts takes the owner brief (fact type owner_brief about
// subject kind database, subject "*") and the do-not-touch fact of
// evid-03b. The single-column CHECKs on fact_type and subject_kind are
// found in pg_constraint, whatever an earlier step named them (the inline
// *_check of ddlFacts or the *_v2 of ddlAgentEnvClass), and replaced once
// by constraints that keep every earlier value. Rows written before keep
// their values, which the new lists include.
//
// The new constraints keep the *_v2 names: ddlAgentEnvClass adds its
// narrower *_v2 lists whenever no constraint of that name exists, and it
// runs again on every boot the ledger does not trust. Under another name
// it would put the old list back (or fail the boot once an owner brief
// row exists). A later widening replaces the *_v2 constraint the same way.
// Idempotent: a second boot finds the *_v2 constraint already naming the
// new value and changes nothing.
const ddlFactsOwnerBrief = `
SET LOCAL lock_timeout = '5s';
DO $$
DECLARE
  col text;
  want text;
  con record;
BEGIN
  FOREACH col IN ARRAY ARRAY['fact_type', 'subject_kind'] LOOP
    want := CASE col WHEN 'fact_type' THEN 'do_not_touch' ELSE 'database' END;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
               WHERE conrelid = 'sage.facts'::regclass
                 AND conname = 'facts_' || col || '_v2'
                 AND strpos(pg_catalog.pg_get_constraintdef(oid),
                            quote_literal(want)) > 0) THEN
      CONTINUE;
    END IF;
    FOR con IN SELECT c.conname FROM pg_catalog.pg_constraint c
                 JOIN pg_catalog.pg_attribute a ON a.attrelid = c.conrelid
                WHERE c.conrelid = 'sage.facts'::regclass AND c.contype = 'c'
                  AND a.attname = col AND c.conkey = ARRAY[a.attnum] LOOP
      EXECUTE format('ALTER TABLE sage.facts DROP CONSTRAINT %I', con.conname);
    END LOOP;
    IF col = 'fact_type' THEN
      ALTER TABLE sage.facts ADD CONSTRAINT facts_fact_type_v2 CHECK (fact_type IN (
          'owned_by_app_migrations', 'test_fixture', 'slot_consumer', 'append_only',
          'table_window', 'column_class', 'owner_brief', 'do_not_touch'));
    ELSE
      ALTER TABLE sage.facts ADD CONSTRAINT facts_subject_kind_v2 CHECK (subject_kind IN (
          'index', 'table', 'schema', 'slot', 'column', 'database'));
    END IF;
  END LOOP;
END $$;
`
