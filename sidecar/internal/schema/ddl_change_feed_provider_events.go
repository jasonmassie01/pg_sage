package schema

// ddlChangeFeedProviderEvents (guard-02b) lets the change feed record
// provider events: a server version change and a host change join the
// kinds pg_sage's own sources emit, and attribution says whether a
// restart, failover, version or host change most likely came from an
// operator, the provider, or is unknown (NULL for other kinds). The new
// kind list is a superset of the old one, so every existing row passes.
const ddlChangeFeedProviderEvents = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.sre_change_events ADD COLUMN IF NOT EXISTS attribution text;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'sre_change_events_kind_v2'
                 AND conrelid = 'sage.sre_change_events'::regclass) THEN
    ALTER TABLE sage.sre_change_events DROP CONSTRAINT IF EXISTS sre_change_events_kind_check;
    ALTER TABLE sage.sre_change_events ADD CONSTRAINT sre_change_events_kind_v2 CHECK (kind IN (
        'deploy', 'migration', 'feature_flag', 'config', 'ddl', 'sage_action',
        'stats_reset', 'restart', 'failover', 'extension', 'other',
        'version_change', 'host_change'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
                 WHERE conname = 'sre_change_events_attribution_v1'
                 AND conrelid = 'sage.sre_change_events'::regclass) THEN
    ALTER TABLE sage.sre_change_events ADD CONSTRAINT sre_change_events_attribution_v1
        CHECK (attribution IN ('operator', 'provider', 'unknown'));
  END IF;
END $$;
`
