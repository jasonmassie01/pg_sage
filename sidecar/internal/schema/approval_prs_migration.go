package schema

// ddlApprovalPRs (cli-02b) records the approval requests pg_sage opens as
// pull requests on a config repository (policy.approval_prs): one row per
// queued change and database, never a second PR for one change. state is
// opening (claimed, being opened), open, open_failed (to be retried at
// next_attempt_at), or a final approved, rejected, refused (the merge was
// not accepted as an approval), superseded (decided elsewhere first) or
// expired. card_hash is the card the PR was opened for and file_hash the
// sha256 of the file it added; a merge approves only while both match.
// The table is new, so lock_timeout only bounds the catalog lock.
const ddlApprovalPRs = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.approval_prs (
    id              bigserial PRIMARY KEY,
    database        text NOT NULL,
    queue_id        integer NOT NULL,
    repo            text NOT NULL,
    path            text NOT NULL,
    pr_number       integer,
    pr_url          text NOT NULL DEFAULT '',
    state           text NOT NULL DEFAULT 'opening'
        CONSTRAINT approval_prs_state CHECK (state IN (
            'opening', 'open', 'open_failed', 'approved', 'rejected', 'refused',
            'superseded', 'expired')),
    card_hash       text NOT NULL,
    file_hash       text NOT NULL DEFAULT '',
    detail          text NOT NULL DEFAULT '',
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,
    expires_at      timestamptz NOT NULL,
    opened_at       timestamptz,
    decided_at      timestamptz,
    decided_by      integer,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT approval_prs_one_per_change UNIQUE (database, queue_id)
);
CREATE INDEX IF NOT EXISTS idx_approval_prs_live ON sage.approval_prs (state)
    WHERE state IN ('opening', 'open', 'open_failed');
`
