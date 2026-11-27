package schema

// ddlDropSizeHistory (forecast-06) drops sage.size_history. Only the v0.9
// growth forecaster wrote it, and that forecaster never had a production
// caller, so the table is empty or holds test rows; growth is now read from
// sage.runway_samples. A fresh bootstrap no longer creates it. The catalog
// check means a re-run takes no lock; lock_timeout bounds the wait of the
// first run.
const ddlDropSizeHistory = `
SET LOCAL lock_timeout = '5s';
DO $drop_size_history$
BEGIN
    IF to_regclass('sage.size_history') IS NOT NULL THEN
        DROP TABLE IF EXISTS sage.size_history;
    END IF;
END
$drop_size_history$;
`
