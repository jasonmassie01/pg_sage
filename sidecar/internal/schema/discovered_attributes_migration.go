package schema

// ddlDiscoveredAttributes (fleetpol-01b) records what a discovery source
// knows about a candidate beyond where it is: its tags (RDS tags, a
// cluster database's pg_sage comment, Neon's project and branch), its
// creator ("agent:..." or "person:...") and the provider's creation
// time; the policy set it matched when last listed; and its enrollment
// milestones (enrolled, first grade asked for, first graded, first at the
// set's baseline). Every column is new and nullable or defaulted, so a
// v2.x row reads as a candidate with no attributes.
const ddlDiscoveredAttributes = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.discovered_databases
    ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(tags) = 'object');
ALTER TABLE sage.discovered_databases
    ADD COLUMN IF NOT EXISTS creator text NOT NULL DEFAULT '';
ALTER TABLE sage.discovered_databases ADD COLUMN IF NOT EXISTS created_at timestamptz;
ALTER TABLE sage.discovered_databases ADD COLUMN IF NOT EXISTS set_name text;
ALTER TABLE sage.discovered_databases ADD COLUMN IF NOT EXISTS enrolled_at timestamptz;
ALTER TABLE sage.discovered_databases
    ADD COLUMN IF NOT EXISTS grade_requested_at timestamptz;
ALTER TABLE sage.discovered_databases ADD COLUMN IF NOT EXISTS first_graded_at timestamptz;
ALTER TABLE sage.discovered_databases ADD COLUMN IF NOT EXISTS baseline_at timestamptz;
CREATE INDEX IF NOT EXISTS discovered_databases_set_idx
    ON sage.discovered_databases (set_name, status) WHERE set_name IS NOT NULL;
`
