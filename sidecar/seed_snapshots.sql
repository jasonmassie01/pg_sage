-- Seed 30 days of historical snapshot data for pg_sage forecaster
-- 3 snapshots/day (8h apart) x 30 days x 3 categories = 270 rows

DELETE FROM sage.snapshots WHERE collected_at < now();

-- ============================================================
-- System snapshots (category = 'system')
-- ============================================================
INSERT INTO sage.snapshots (collected_at, category, data)
SELECT
    ts,
    'system',
    jsonb_build_object(
        'db_size_bytes',    ((5368709120::bigint + day_num * 209715200::bigint) * (1 + (random() - 0.5) * 0.04))::bigint,
        'active_backends',  ((15 + day_num * 0.5) * (1 + (random() - 0.5) * 0.04))::int,
        'total_backends',   ((20 + day_num * 0.5) * (1 + (random() - 0.5) * 0.04))::int,
        'max_connections',  100,
        'cache_hit_ratio',  round(((99.5 - day_num * 0.15) * (1 + (random() - 0.5) * 0.04))::numeric, 2),
        'total_checkpoints', ((1000 + day_num * 300) * (1 + (random() - 0.5) * 0.04))::bigint
    )
FROM (
    SELECT
        now() - interval '30 days' + (d * interval '1 day') + (s * interval '8 hours') AS ts,
        d AS day_num
    FROM generate_series(0, 29) AS d,
         generate_series(0, 2) AS s
) AS t;

-- ============================================================
-- Query snapshots (category = 'queries')
-- Week 1: ~50000/day, Week 2: ~75000/day, Week 3: ~120000/day, Week 4: ~200000/day
-- ============================================================
INSERT INTO sage.snapshots (collected_at, category, data)
SELECT
    ts,
    'queries',
    jsonb_build_array(
        jsonb_build_object(
            'queryid', 7216849732456821::bigint,
            'calls',   round(daily_calls * 0.35 * (1 + (random() - 0.5) * 0.04))
        ),
        jsonb_build_object(
            'queryid', 1498230517643982::bigint,
            'calls',   round(daily_calls * 0.25 * (1 + (random() - 0.5) * 0.04))
        ),
        jsonb_build_object(
            'queryid', 3852917406285103::bigint,
            'calls',   round(daily_calls * 0.20 * (1 + (random() - 0.5) * 0.04))
        ),
        jsonb_build_object(
            'queryid', 5603184927561047::bigint,
            'calls',   round(daily_calls * 0.12 * (1 + (random() - 0.5) * 0.04))
        ),
        jsonb_build_object(
            'queryid', 9374025816390274::bigint,
            'calls',   round(daily_calls * 0.08 * (1 + (random() - 0.5) * 0.04))
        )
    )
FROM (
    SELECT
        now() - interval '30 days' + (d * interval '1 day') + (s * interval '8 hours') AS ts,
        CASE
            WHEN d < 7  THEN 50000.0 + d * 3571.0
            WHEN d < 14 THEN 75000.0 + (d - 7) * 6428.0
            WHEN d < 21 THEN 120000.0 + (d - 14) * 11428.0
            ELSE             200000.0 + (d - 21) * 5000.0
        END AS daily_calls
    FROM generate_series(0, 29) AS d,
         generate_series(0, 2) AS s
) AS t;

-- ============================================================
-- Sequence snapshots (category = 'sequences')
-- orders_id_seq: 75% start, +0.8%/day (~31 days to exhaust)
-- events_id_seq: 90% start, +0.5%/day (~20 days to exhaust)
-- ============================================================
INSERT INTO sage.snapshots (collected_at, category, data)
SELECT
    ts,
    'sequences',
    jsonb_build_array(
        jsonb_build_object(
            'schemaname',   'public',
            'sequencename', 'orders_id_seq',
            'pct_used',     round(least(100.0, (75.0 + day_num * 0.8) * (1 + (random() - 0.5) * 0.04))::numeric, 2),
            'max_value',    2147483647
        ),
        jsonb_build_object(
            'schemaname',   'public',
            'sequencename', 'events_id_seq',
            'pct_used',     round(least(100.0, (90.0 + day_num * 0.5) * (1 + (random() - 0.5) * 0.04))::numeric, 2),
            'max_value',    2147483647
        )
    )
FROM (
    SELECT
        now() - interval '30 days' + (d * interval '1 day') + (s * interval '8 hours') AS ts,
        d AS day_num
    FROM generate_series(0, 29) AS d,
         generate_series(0, 2) AS s
) AS t;
