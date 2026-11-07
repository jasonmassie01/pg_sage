package schema

// ddlSessionsTokenHash stores sessions by the SHA-256 of their cookie
// (apiauth-04). Before it, sage.sessions.id was the cookie itself, so any
// reader of the sage schema could lift a live session. Every session
// without a hash is deleted: the one forced re-login at upgrade (sessions
// last at most 24 h). id stays as a non-secret row key. Each step checks
// the catalog first, so once applied a re-run takes no lock on
// sage.sessions and a restart never waits behind session traffic;
// lock_timeout bounds the wait of the first run.
const ddlSessionsTokenHash = `
SET LOCAL lock_timeout = '5s';
DO $sessions_token_hash$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.sessions'::regclass
           AND attname = 'token_hash' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.sessions ADD COLUMN token_hash TEXT;
    END IF;
    IF EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.sessions'::regclass
           AND attname = 'token_hash' AND NOT attisdropped AND NOT attnotnull
    ) THEN
        DELETE FROM sage.sessions WHERE token_hash IS NULL;
        ALTER TABLE sage.sessions ALTER COLUMN token_hash SET NOT NULL;
    END IF;
    IF to_regclass('sage.idx_sessions_token_hash') IS NULL THEN
        CREATE UNIQUE INDEX idx_sessions_token_hash
            ON sage.sessions (token_hash);
    END IF;
END
$sessions_token_hash$;
`
