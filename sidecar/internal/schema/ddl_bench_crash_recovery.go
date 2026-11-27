package schema

// ddlBenchCrashRecovery (benches-03) lets a crashed bench run be cleaned
// up by the next one:
//
//   - sage.sre_game_days.kind tells a scheduled game day ('game_day', every
//     existing row) from a local bench run ('local'), which is now
//     recorded too;
//   - the terminal status 'abandoned' is a run its process never finished
//     (marked once it is older than the maximum run time, and its clone
//     destroyed);
//   - sage.bench_fixture_state, on a bench target, names each cluster
//     setting a fault program changed with ALTER SYSTEM until it is reset,
//     so the next run resets what a crashed one left.
//
// The catalog checks mean a re-run takes no lock on sage.sre_game_days;
// lock_timeout bounds the wait of the first run.
const ddlBenchCrashRecovery = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.bench_fixture_state (
    setting text PRIMARY KEY CHECK (setting ~ '^[a-z][a-z0-9_.]{0,62}$'),
    set_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
DO $bench_crash_recovery$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.sre_game_days'::regclass
           AND attname = 'kind' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.sre_game_days ADD COLUMN kind text NOT NULL DEFAULT 'game_day'
            CONSTRAINT sre_game_days_kind_check CHECK (kind IN ('game_day', 'local'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.sre_game_days'::regclass
           AND conname = 'sre_game_days_status_v2_check'
    ) THEN
        ALTER TABLE sage.sre_game_days DROP CONSTRAINT IF EXISTS sre_game_days_status_check;
        ALTER TABLE sage.sre_game_days ADD CONSTRAINT sre_game_days_status_v2_check
            CHECK (status IN ('running', 'completed', 'failed', 'destroy_failed',
                              'abandoned'));
    END IF;
END
$bench_crash_recovery$;
`
