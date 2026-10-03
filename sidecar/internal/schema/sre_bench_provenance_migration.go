package schema

// Roadmap 1.1 (2026-10-03) bench provenance: a stored PGIncidentBench
// report keeps where it came from (origin: signed_release, local_run,
// operator, game_day), the pg_sage build it scored and, when signed, the
// verified signature. Reports stored before are operator reports (bench)
// or game days. Additive and idempotent on top of ddlSREAutonomy.
const ddlSREBenchProvenance = `
ALTER TABLE sage.sre_eval_runs
    ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT 'operator',
    ADD COLUMN IF NOT EXISTS pg_sage_version text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pg_sage_commit text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS signature jsonb;

UPDATE sage.sre_eval_runs SET origin = 'game_day'
WHERE source = 'game_day' AND origin <> 'game_day';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_eval_runs_origin'
                     AND conrelid = 'sage.sre_eval_runs'::regclass) THEN
        ALTER TABLE sage.sre_eval_runs
            ADD CONSTRAINT sre_eval_runs_origin CHECK (
                origin IN ('signed_release', 'local_run', 'operator', 'game_day')
                AND (origin = 'signed_release') = (signature IS NOT NULL)
                AND length(pg_sage_version) <= 64 AND length(pg_sage_commit) <= 64);
    END IF;
END $$;
`
