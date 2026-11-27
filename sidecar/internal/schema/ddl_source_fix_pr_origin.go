package schema

// ddlSourceFixPROrigin (pr-01b) records who opened a source fix's pull
// request: origin 'agent' (a coding agent's report, every row before this
// step) or 'pg_sage' (PR mode), with the repository, the PR number and
// its last seen state (open, closed unmerged, merged, or a revert PR
// opened after a regressed verdict). The catalog check first means a
// re-run takes no lock on sage.source_fix; lock_timeout bounds the first.
// Adding a column with a constant default rewrites no rows.
const ddlSourceFixPROrigin = `
SET LOCAL lock_timeout = '5s';
DO $source_fix_pr_origin$
BEGIN
    IF (SELECT count(*) FROM pg_attribute
         WHERE attrelid = 'sage.source_fix'::regclass AND NOT attisdropped
           AND attname IN ('origin', 'repo', 'pr_number', 'pr_state')) < 4 THEN
        ALTER TABLE sage.source_fix
            ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT 'agent'
                CHECK (origin IN ('agent', 'pg_sage')),
            ADD COLUMN IF NOT EXISTS repo text CHECK (length(repo) BETWEEN 1 AND 300),
            ADD COLUMN IF NOT EXISTS pr_number integer CHECK (pr_number > 0),
            ADD COLUMN IF NOT EXISTS pr_state text
                CHECK (pr_state IN ('open', 'closed', 'merged', 'revert_opened'));
    END IF;
END
$source_fix_pr_origin$;
`
