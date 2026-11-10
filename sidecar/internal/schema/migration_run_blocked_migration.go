package schema

// ddlMigrationRunBlocked (migration-03) lets sage.migration_run record the
// "blocked" verdict an MCP apply_migration writes when the policy gate
// refuses it (the old CHECK rejected it with 23514, so the block surfaced
// as an error), and adds the reason column that carries the gate's reason.
//
// The verdict CHECK is found through pg_constraint by the column it covers,
// not by an assumed name. Only a CHECK whose definition lacks 'blocked' is
// dropped; a re-run finds the column and the new CHECK and takes no lock.
// The first run briefly holds ACCESS EXCLUSIVE on this low-volume table,
// bounded by lock_timeout; re-adding the CHECK validates existing rows,
// which all hold one of the old verdicts.
const ddlMigrationRunBlocked = `
SET LOCAL lock_timeout = '5s';
DO $migration_run_blocked$
DECLARE c record;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.migration_run'::regclass
           AND attname = 'reason' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.migration_run ADD COLUMN reason text NOT NULL DEFAULT '';
    END IF;
    FOR c IN SELECT con.conname FROM pg_constraint con
               JOIN pg_attribute a ON a.attrelid = con.conrelid
                AND a.attnum = ANY (con.conkey)
              WHERE con.conrelid = 'sage.migration_run'::regclass
                AND con.contype = 'c' AND a.attname = 'verdict'
                AND pg_get_constraintdef(con.oid) NOT LIKE '%''blocked''%'
    LOOP
        EXECUTE format('ALTER TABLE sage.migration_run DROP CONSTRAINT %I', c.conname);
    END LOOP;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint con
          JOIN pg_attribute a ON a.attrelid = con.conrelid
           AND a.attnum = ANY (con.conkey)
         WHERE con.conrelid = 'sage.migration_run'::regclass
           AND con.contype = 'c' AND a.attname = 'verdict'
    ) THEN
        ALTER TABLE sage.migration_run ADD CONSTRAINT migration_run_verdict_check
            CHECK (verdict IN ('pending', 'recommend_only', 'parked', 'promote',
                               'failed', 'blocked'));
    END IF;
END
$migration_run_blocked$;
`
