\set ON_ERROR_STOP on

DO $$
BEGIN
  IF to_regnamespace('sage') IS NULL THEN
    RETURN;
  END IF;

  IF to_regclass('sage.action_queue') IS NOT NULL THEN
    DELETE FROM sage.action_queue;
  END IF;
  IF to_regclass('sage.findings') IS NOT NULL THEN
    UPDATE sage.findings
       SET action_log_id = NULL
     WHERE action_log_id IS NOT NULL;
  END IF;
  IF to_regclass('sage.notification_log') IS NOT NULL THEN
    DELETE FROM sage.notification_log;
  END IF;
  IF to_regclass('sage.alert_log') IS NOT NULL THEN
    DELETE FROM sage.alert_log;
  END IF;
  IF to_regclass('sage.query_hints') IS NOT NULL THEN
    DELETE FROM sage.query_hints;
  END IF;
  IF to_regclass('sage.explain_cache') IS NOT NULL THEN
    DELETE FROM sage.explain_cache;
  END IF;
  IF to_regclass('sage.findings') IS NOT NULL THEN
    DELETE FROM sage.findings;
  END IF;
  IF to_regclass('sage.action_log') IS NOT NULL THEN
    DELETE FROM sage.action_log;
  END IF;
  IF to_regclass('sage.config') IS NOT NULL THEN
    DELETE FROM sage.config WHERE key = 'trust_ramp_start';
  END IF;
END $$;
