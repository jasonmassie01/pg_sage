package schema

// ddlGameDayExercised (benches-12) records what a game day exercised in
// sage.sre_game_days.exercised: the clone provider, whether the clone
// holds customer objects, each scenario of the run's report and each
// customer program's binding (the table it locked) or skip reason. NULL is
// a row written before this step, or a local bench run. The table name
// stays in this local row; exported reports carry its hash only.
//
// ADD COLUMN IF NOT EXISTS of a nullable column without a default is a
// catalog-only change; the check is added only when it is missing, so a
// re-run takes no lock. lock_timeout bounds the first run's wait. Old
// binaries ignore the column.
const ddlGameDayExercised = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.sre_game_days ADD COLUMN IF NOT EXISTS exercised jsonb;
DO $game_day_exercised$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.sre_game_days'::regclass
           AND conname = 'sre_game_days_exercised_object'
    ) THEN
        ALTER TABLE sage.sre_game_days ADD CONSTRAINT sre_game_days_exercised_object
            CHECK (exercised IS NULL OR jsonb_typeof(exercised) = 'object');
    END IF;
END
$game_day_exercised$;
`
