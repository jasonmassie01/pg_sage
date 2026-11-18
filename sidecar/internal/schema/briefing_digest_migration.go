package schema

// ddlBriefingsDigestHash (ux-02b) stores each briefing's digest hash and,
// per scope (one database, or the fleet roll-up), the hash last delivered:
// the scheduled digest is sent only when its hash differs, and the claim
// is one INSERT ... ON CONFLICT (scope) so two leaders never both send.
const ddlBriefingsDigestHash = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.briefings ADD COLUMN IF NOT EXISTS digest_hash text;
CREATE TABLE IF NOT EXISTS sage.digest_delivery (
    scope       text PRIMARY KEY CHECK (scope <> ''),
    digest_hash text NOT NULL CHECK (digest_hash ~ '^[0-9a-f]{64}$'),
    briefing_id bigint,
    delivered_at timestamptz NOT NULL DEFAULT now()
);
`
