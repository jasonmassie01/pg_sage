package schema

// ddlGameDaySummary (benches-08) records what a game day or local bench
// run scored:
//
//   - sage.sre_game_days.summary holds the counts of scored, skipped and
//     errored runs and the top skip reasons ('{}' for a row written before
//     this step, or a run that failed before any report);
//   - the terminal status 'completed_unscored' is a run that finished but
//     scored no run (every fault program skipped, or the report had none).
//
// sre_game_days_status_v2_check is redefined as the superset of every
// earlier status plus the new one (the drop-and-add pattern of
// ddlSREAutonomyRootAuthority), so benches-03's guard, which looks for the
// constraint by name, keeps skipping. The catalog checks mean a re-run
// takes no lock; lock_timeout bounds the wait of the first run. Old
// binaries never write the new status and ignore the column.
const ddlGameDaySummary = `
SET LOCAL lock_timeout = '5s';
DO $game_day_summary$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.sre_game_days'::regclass
           AND attname = 'summary' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.sre_game_days ADD COLUMN summary jsonb NOT NULL DEFAULT '{}'
            CONSTRAINT sre_game_days_summary_object CHECK (jsonb_typeof(summary) = 'object');
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.sre_game_days'::regclass
           AND conname = 'sre_game_days_status_v2_check'
           AND pg_get_constraintdef(oid) LIKE '%completed_unscored%'
    ) THEN
        ALTER TABLE sage.sre_game_days DROP CONSTRAINT IF EXISTS sre_game_days_status_v2_check;
        ALTER TABLE sage.sre_game_days DROP CONSTRAINT IF EXISTS sre_game_days_status_check;
        ALTER TABLE sage.sre_game_days ADD CONSTRAINT sre_game_days_status_v2_check
            CHECK (status IN ('running', 'completed', 'completed_unscored', 'failed',
                              'destroy_failed', 'abandoned'));
    END IF;
END
$game_day_summary$;
`
