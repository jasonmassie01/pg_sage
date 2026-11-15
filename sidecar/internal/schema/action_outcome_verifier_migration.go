package schema

// ddlActionOutcomeVerifier (lab-01c) records which verifier decided an
// outcome and the other one's verdict, so v1 and v2 can be compared while
// v2 runs in shadow. The trust ledger keeps reading verdict (the acted-on
// one) only, so trust and demotion are not counted twice. ADD COLUMN IF
// NOT EXISTS takes no rewrite (constant default); lock_timeout bounds the
// first run's wait.
const ddlActionOutcomeVerifier = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.action_outcome
    ADD COLUMN IF NOT EXISTS verifier text NOT NULL DEFAULT 'v1'
        CONSTRAINT action_outcome_verifier CHECK (verifier IN ('v1', 'v2')),
    ADD COLUMN IF NOT EXISTS other_verdict text,
    ADD COLUMN IF NOT EXISTS other_evidence jsonb;
`
