package schema

// ddlDatabasesDiscovered (market-06) adds the discovery columns to
// sage.databases and the guard that keeps an unadopted discovered member
// at trust observation with approval (or manual) execution, whatever path
// writes the row. SQLSTATE SGD01 maps to store.ErrDiscoveredTrust.
const ddlDatabasesDiscovered = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.databases ADD COLUMN IF NOT EXISTS discovered_source TEXT;
ALTER TABLE sage.databases ADD COLUMN IF NOT EXISTS owner TEXT;
ALTER TABLE sage.databases ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE sage.databases ADD COLUMN IF NOT EXISTS adopted_at TIMESTAMPTZ;
ALTER TABLE sage.databases ADD COLUMN IF NOT EXISTS adopted_by INT;
CREATE OR REPLACE FUNCTION sage.databases_discovered_trust_guard()
RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.discovered_source IS NOT NULL
       AND NEW.discovered_source IS DISTINCT FROM OLD.discovered_source THEN
        RAISE EXCEPTION 'database % was discovered by %; its source cannot change',
            OLD.name, OLD.discovered_source USING ERRCODE = 'SGD01';
    END IF;
    IF NEW.discovered_source IS NOT NULL AND NEW.adopted_at IS NULL
       AND (NEW.trust_level IS DISTINCT FROM 'observation'
            OR NEW.execution_mode NOT IN ('approval', 'manual')) THEN
        RAISE EXCEPTION 'discovered database % needs an owner to adopt it first', NEW.name
            USING ERRCODE = 'SGD01';
    END IF;
    RETURN NEW;
END
$guard$;
DO $guard_trigger$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
         WHERE tgrelid = 'sage.databases'::regclass
           AND tgname = 'databases_discovered_trust_guard' AND NOT tgisinternal
    ) THEN
        CREATE TRIGGER databases_discovered_trust_guard
            BEFORE INSERT OR UPDATE ON sage.databases
            FOR EACH ROW EXECUTE FUNCTION sage.databases_discovered_trust_guard();
    END IF;
END
$guard_trigger$;
`

// ddlDiscoveredDatabases (market-06) records what each discovery source
// reported: one row per (source, external_id), and each source's last run.
const ddlDiscoveredDatabases = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.discovered_databases (
    id                BIGSERIAL PRIMARY KEY,
    source            TEXT NOT NULL,
    external_id       TEXT NOT NULL,
    host              TEXT NOT NULL DEFAULT '',
    port              INT NOT NULL DEFAULT 5432,
    dbname            TEXT NOT NULL DEFAULT '',
    owner             TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'candidate'
        CONSTRAINT discovered_databases_status_check CHECK (status IN
            ('candidate', 'enrolling', 'enrolled', 'adopted', 'expired', 'ignored',
             'gone')),
    database_id       INT REFERENCES sage.databases(id) ON DELETE SET NULL,
    last_error        TEXT NOT NULL DEFAULT '',
    first_seen        TIMESTAMPTZ NOT NULL,
    last_seen         TIMESTAMPTZ NOT NULL,
    status_changed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT discovered_databases_source_external_key UNIQUE (source, external_id)
);
CREATE INDEX IF NOT EXISTS discovered_databases_status_idx
    ON sage.discovered_databases (status, source);
CREATE INDEX IF NOT EXISTS discovered_databases_database_id_idx
    ON sage.discovered_databases (database_id);
CREATE TABLE IF NOT EXISTS sage.discovery_sources (
    source          TEXT PRIMARY KEY,
    last_run_at     TIMESTAMPTZ NOT NULL,
    last_success_at TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    last_count      INT NOT NULL DEFAULT 0
);
`
