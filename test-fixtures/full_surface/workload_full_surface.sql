\set ON_ERROR_STOP on
SET search_path = sage_verify, public;

DO $$
BEGIN
  BEGIN
    PERFORM pg_stat_statements_reset();
  EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'pg_stat_statements reset skipped: %', SQLERRM;
  END;
END $$;

SELECT pg_sleep(0.02);
SELECT pg_sleep(0.02);
SELECT pg_sleep(0.02);

DO $$
BEGIN
  FOR i IN 1..130 LOOP
    PERFORM count(*) FROM sage_verify.orders WHERE amount = -1;
  END LOOP;
END $$;

DO $$
BEGIN
  FOR i IN 1..40 LOOP
    PERFORM * FROM sage_verify.sort_target ORDER BY score DESC LIMIT 10;
  END LOOP;
END $$;

DO $$
BEGIN
  FOR i IN 1..30 LOOP
    PERFORM count(*)
      FROM sage_verify.join_left l
      JOIN sage_verify.join_right r ON r.join_key = l.join_key
     WHERE l.id BETWEEN 1 AND 2000;
  END LOOP;
END $$;

-- pg_stat_statements records a PL/pgSQL DO block as one statement, not
-- one call per inner PERFORM. Use psql \gexec so the collector sees a
-- deterministic high-frequency query for high_total_time coverage.
\o /dev/null
SELECT format(
  'SELECT id FROM sage_verify.customers WHERE id = %s;',
  (i % 5000) + 1
)
FROM generate_series(1, 12000) AS g(i)
\gexec
\o

SET work_mem = '64kB';
SELECT count(*) FROM (SELECT * FROM sage_verify.sort_target ORDER BY payload, score) s;
RESET work_mem;

DO $$
DECLARE
  qid bigint;
BEGIN
  IF to_regclass('sage.explain_cache') IS NULL THEN
    RAISE NOTICE 'sage.explain_cache missing; sidecar bootstrap has not run yet';
    RETURN;
  END IF;

  SELECT queryid INTO qid
    FROM pg_stat_statements
   WHERE query LIKE '%sage_verify.sort_target%'
     AND query LIKE '%ORDER BY score%'
   ORDER BY calls DESC
   LIMIT 1;

  IF qid IS NOT NULL THEN
    INSERT INTO sage.explain_cache (queryid, query_text, plan_json, source)
    VALUES (
      qid,
      'SELECT * FROM sage_verify.sort_target ORDER BY score DESC LIMIT 10',
      '[{"Plan":{"Node Type":"Limit","Plan Rows":10,"Plans":[{"Node Type":"Sort","Plan Rows":15000,"Sort Key":["score DESC"],"Sort Method":"top-N heapsort","Sort Space Type":"Memory","Plans":[{"Node Type":"Seq Scan","Schema":"sage_verify","Relation Name":"sort_target","Alias":"sort_target","Plan Rows":15000,"Actual Rows":15000}]}]}}]'::jsonb,
      'full_surface_fixture'
    );
  END IF;

  SELECT queryid INTO qid
    FROM pg_stat_statements
   WHERE query LIKE '%sage_verify.join_left%'
     AND query LIKE '%sage_verify.join_right%'
   ORDER BY calls DESC
   LIMIT 1;

  IF qid IS NOT NULL THEN
    INSERT INTO sage.explain_cache (queryid, query_text, plan_json, source)
    VALUES (
      qid,
      'SELECT count(*) FROM sage_verify.join_left l JOIN sage_verify.join_right r ON r.join_key = l.join_key WHERE l.id BETWEEN 1 AND 2000',
      '[{"Plan":{"Node Type":"Nested Loop","Plan Rows":10,"Actual Rows":50000,"Plans":[{"Node Type":"Seq Scan","Schema":"sage_verify","Relation Name":"join_left","Alias":"l","Plan Rows":2000,"Actual Rows":2000},{"Node Type":"Seq Scan","Schema":"sage_verify","Relation Name":"join_right","Alias":"r","Plan Rows":5000,"Actual Rows":5000,"Index Name":"idx_verify_join_right_key"}]}}]'::jsonb,
      'full_surface_fixture'
    );
  END IF;

  SELECT queryid INTO qid
    FROM pg_stat_statements
   WHERE query LIKE '%sage_verify.sort_target%'
     AND query LIKE '%ORDER BY payload%'
   ORDER BY calls DESC
   LIMIT 1;

  IF qid IS NOT NULL THEN
    INSERT INTO sage.explain_cache (queryid, query_text, plan_json, source)
    VALUES (
      qid,
      'SELECT count(*) FROM (SELECT * FROM sage_verify.sort_target ORDER BY payload, score) s',
      '[{"Plan":{"Node Type":"Sort","Plan Rows":15000,"Sort Method":"external merge","Sort Space Used":8192,"Sort Space Type":"Disk","Plans":[{"Node Type":"Hash Join","Plan Rows":1000,"Hash Batches":8,"Original Hash Batches":1,"Peak Memory Usage":4096,"Plans":[{"Node Type":"Seq Scan","Schema":"sage_verify","Relation Name":"join_left","Alias":"l","Plan Rows":5000},{"Node Type":"Hash","Plan Rows":5000,"Plans":[{"Node Type":"Seq Scan","Schema":"sage_verify","Relation Name":"join_right","Alias":"r","Plan Rows":5000}]}]}]}}]'::jsonb,
      'full_surface_fixture'
    );
  END IF;
EXCEPTION WHEN OTHERS THEN
  RAISE NOTICE 'explain_cache seeding skipped: %', SQLERRM;
END $$;
