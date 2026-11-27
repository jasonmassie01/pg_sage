package schema

// ddlEmergencyStopSeed (fleet-09) seeds the global emergency_stop flag as
// 'false', attributed to bootstrap, so the documented
// UPDATE sage.config SET value = 'true' WHERE key = 'emergency_stop'
// matches a row on a database whose stop was never toggled. It never
// overwrites an existing row: ON CONFLICT DO NOTHING (no conflict target,
// because on a fresh install this runs before the config migration replaces
// the key primary key with the (key, database_id) unique index), and the
// NOT EXISTS keeps a rerun from adding a second global row.
const ddlEmergencyStopSeed = `
SET LOCAL lock_timeout = '5s';
INSERT INTO sage.config (key, value, updated_at, updated_by)
SELECT 'emergency_stop', 'false', now(), 'bootstrap'
WHERE NOT EXISTS (
    SELECT 1 FROM sage.config WHERE key = 'emergency_stop'
)
ON CONFLICT DO NOTHING;
`
