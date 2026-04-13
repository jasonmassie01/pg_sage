-- seed_incidents.sql — Inserts example incidents for every RCA case
-- so you can log in and see Tier 1 + Tier 2 incidents in the UI.
--
-- Usage: psql -f scripts/seed_incidents.sql <your_database>
--
-- Safe to re-run: uses ON CONFLICT DO NOTHING with fixed UUIDs.

-- Ensure the migration has run (expanded CHECK constraints).
DO $$ BEGIN
    ALTER TABLE sage.incidents DROP CONSTRAINT IF EXISTS incidents_severity_check;
    ALTER TABLE sage.incidents ADD CONSTRAINT incidents_severity_check
        CHECK (severity IN ('info', 'warning', 'critical'));

    ALTER TABLE sage.incidents DROP CONSTRAINT IF EXISTS incidents_source_check;
    ALTER TABLE sage.incidents ADD CONSTRAINT incidents_source_check
        CHECK (source IN (
            'deterministic', 'log_deterministic',
            'self_action', 'manual_review_required', 'llm'
        ));

    ALTER TABLE sage.incidents DROP CONSTRAINT IF EXISTS incidents_action_risk_check;
    ALTER TABLE sage.incidents ADD CONSTRAINT incidents_action_risk_check
        CHECK (action_risk IN (
            'safe', 'moderate', 'high_risk', 'low', 'medium', 'high'
        ) OR action_risk IS NULL);
END $$;

-- ===========================================================================
-- TIER 1: Metric-based deterministic incidents
-- ===========================================================================

INSERT INTO sage.incidents (
    id, detected_at, severity, root_cause, causal_chain,
    affected_objects, signal_ids, recommended_sql, action_risk,
    source, confidence, database_name, occurrence_count
) VALUES

-- 1. Connections high + idle-in-tx
('a0000001-0001-4000-8000-000000000001', now() - interval '25 minutes',
 'critical',
 'Idle-in-transaction sessions saturating connection pool',
 '[{"order":1,"signal":"idle_in_tx_elevated","description":"Sessions stuck in idle-in-transaction state","evidence":"12 idle-in-tx sessions"},{"order":2,"signal":"connections_high","description":"Connection slots exhausted by idle sessions","evidence":"92/100 connections used"}]',
 ARRAY['pg_stat_activity'], ARRAY['connections_high','idle_in_tx_elevated'],
 'SELECT pid, state, query, now()-state_change AS duration FROM pg_stat_activity WHERE state = ''idle in transaction'' ORDER BY state_change LIMIT 20;',
 'moderate', 'deterministic', 0.85, 'production', 3),

-- 2. Connection storm
('a0000001-0001-4000-8000-000000000002', now() - interval '18 minutes',
 'warning',
 'Connection storm: rapid connection creation/teardown',
 '[{"order":1,"signal":"connections_high","description":"Connection churn doubled since last cycle","evidence":"Churn: 450 (prev 180)"}]',
 ARRAY['pg_stat_activity'], ARRAY['connections_high'],
 NULL, 'moderate', 'deterministic', 0.85, 'production', 2),

-- 3. Cache hit ratio drop (query evicting buffers)
('a0000001-0001-4000-8000-000000000003', now() - interval '12 minutes',
 'warning',
 'Query 8837261 reading excessive blocks, evicting shared_buffers',
 '[{"order":1,"signal":"cache_hit_ratio_drop","description":"Specific query read 10x+ more blocks","evidence":"QueryID 8837261: 520000 blks (prev 42000) -- SELECT * FROM orders JOIN line_items..."}]',
 ARRAY['query:8837261'], ARRAY['cache_hit_ratio_drop'],
 NULL, 'safe', 'deterministic', 0.85, 'production', 1),

-- 4. Vacuum blocked by idle-in-tx
('a0000001-0001-4000-8000-000000000004', now() - interval '35 minutes',
 'warning',
 'Idle-in-transaction PID 15234 holding oldest xmin, blocking autovacuum',
 '[{"order":1,"signal":"vacuum_blocked","description":"Session holding transaction open prevents dead-tuple cleanup","evidence":"PID 15234 in state: idle in transaction"}]',
 ARRAY['public.orders','public.line_items'], ARRAY['vacuum_blocked'],
 'SELECT pg_terminate_backend(15234); -- idle-in-tx blocker',
 'high_risk', 'deterministic', 0.85, 'production', 5),

-- 5. Replication lag
('a0000001-0001-4000-8000-000000000005', now() - interval '8 minutes',
 'warning',
 'Replication lag exceeds threshold',
 '[{"order":1,"signal":"replication_lag_increasing","description":"Replication lag exceeds threshold","evidence":"Worst replica lag: 45s"}]',
 NULL, ARRAY['replication_lag_increasing'],
 NULL, 'safe', 'deterministic', 0.85, 'production', 2),

-- 6. Lock contention
('a0000001-0001-4000-8000-000000000006', now() - interval '5 minutes',
 'warning',
 'Lock chain contention detected',
 '[{"order":1,"signal":"lock_contention","description":"Lock chain contention detected","evidence":"3 lock chains, 12 total blocked"}]',
 NULL, ARRAY['lock_contention'],
 NULL, 'safe', 'deterministic', 0.85, 'production', 1),

-- 7. WAL spike
('a0000001-0001-4000-8000-000000000007', now() - interval '15 minutes',
 'warning',
 'WAL generation spiked 3.2x over previous cycle',
 '[{"order":1,"signal":"wal_growth_spike","description":"WAL generation spiked 3.2x over previous cycle","evidence":"Current: 524288000 bytes, Previous: 163840000 bytes"}]',
 NULL, ARRAY['wal_growth_spike'],
 NULL, 'safe', 'deterministic', 0.85, 'production', 1)

ON CONFLICT (id) DO NOTHING;

-- ===========================================================================
-- TIER 1: Log-based deterministic incidents
-- ===========================================================================

INSERT INTO sage.incidents (
    id, detected_at, severity, root_cause, causal_chain,
    affected_objects, signal_ids, recommended_sql, action_risk,
    source, confidence, database_name, occurrence_count
) VALUES

-- 8. Deadlock
('b0000001-0001-4000-8000-000000000001', now() - interval '22 minutes',
 'critical',
 'Deadlock detected between concurrent transactions',
 '[{"order":1,"signal":"log_deadlock_detected","description":"Deadlock detected between concurrent transactions","evidence":"database=production, pids=15234,15267 [self-inflicted deadlock]"}]',
 ARRAY['production'], ARRAY['log_deadlock_detected'],
 NULL, 'moderate', 'log_deterministic', 0.85, 'production', 2),

-- 9. Connection refused
('b0000001-0001-4000-8000-000000000002', now() - interval '30 minutes',
 'critical',
 'Connection refused: too many clients already',
 '[{"order":1,"signal":"log_connection_refused","description":"Connection refused: too many clients already","evidence":"too many clients already"}]',
 NULL, ARRAY['log_connection_refused'],
 NULL, 'high_risk', 'log_deterministic', 0.85, 'production', 4),

-- 10. Out of memory
('b0000001-0001-4000-8000-000000000003', now() - interval '10 minutes',
 'critical',
 'Out of memory during query execution',
 '[{"order":1,"signal":"log_out_of_memory","description":"Out of memory during query execution","evidence":"database=analytics: Out of memory during query execution"}]',
 ARRAY['analytics'], ARRAY['log_out_of_memory'],
 NULL, 'high_risk', 'log_deterministic', 0.85, 'analytics', 1),

-- 11. Disk full
('b0000001-0001-4000-8000-000000000004', now() - interval '3 minutes',
 'critical',
 'Disk full: no space left on device',
 '[{"order":1,"signal":"log_disk_full","description":"Disk full: no space left on device","evidence":"PANIC: could not write to file pg_wal/00000001000000030000002A: No space left on device"}]',
 NULL, ARRAY['log_disk_full'],
 NULL, 'high_risk', 'log_deterministic', 0.85, 'production', 1),

-- 12. Server crash
('b0000001-0001-4000-8000-000000000005', now() - interval '45 minutes',
 'critical',
 'PostgreSQL server crash (PANIC or signal)',
 '[{"order":1,"signal":"log_panic_server_crash","description":"PostgreSQL server crash (PANIC or signal)","evidence":"server process (PID 12345) was terminated by signal 11: Segmentation fault"}]',
 NULL, ARRAY['log_panic_server_crash'],
 NULL, 'high_risk', 'log_deterministic', 0.85, 'production', 1),

-- 13. Data corruption
('b0000001-0001-4000-8000-000000000006', now() - interval '1 hour',
 'critical',
 'Data corruption detected (class XX error)',
 '[{"order":1,"signal":"log_data_corruption","description":"Data corruption detected (class XX error)","evidence":"invalid page in block 42 of relation base/16384/24576"}]',
 NULL, ARRAY['log_data_corruption'],
 NULL, 'high_risk', 'log_deterministic', 0.85, 'production', 1),

-- 14. TXID wraparound
('b0000001-0001-4000-8000-000000000007', now() - interval '20 minutes',
 'critical',
 'Transaction ID wraparound imminent -- emergency VACUUM required',
 '[{"order":1,"signal":"log_txid_wraparound_warning","description":"Transaction ID wraparound imminent -- emergency VACUUM required","evidence":"database production must be vacuumed within 1000000 transactions"}]',
 NULL, ARRAY['log_txid_wraparound_warning'],
 'SELECT datname, age(datfrozenxid) FROM pg_database ORDER BY age(datfrozenxid) DESC;',
 'high_risk', 'log_deterministic', 0.85, 'production', 1),

-- 15. Archive failed
('b0000001-0001-4000-8000-000000000008', now() - interval '40 minutes',
 'critical',
 'WAL archive command failed',
 '[{"order":1,"signal":"log_archive_failed","description":"WAL archive command failed","evidence":"archive command failed with exit code 1"}]',
 NULL, ARRAY['log_archive_failed'],
 NULL, 'moderate', 'log_deterministic', 0.85, 'production', 3),

-- 16. Temp file
('b0000001-0001-4000-8000-000000000009', now() - interval '7 minutes',
 'warning',
 'Large temporary file created (possible work_mem undersize)',
 '[{"order":1,"signal":"log_temp_file_created","description":"Large temporary file created (possible work_mem undersize)","evidence":"temp_file_bytes=104857600"}]',
 NULL, ARRAY['log_temp_file_created'],
 NULL, 'safe', 'log_deterministic', 0.85, 'production', 2),

-- 17. Checkpoint too frequent
('b0000001-0001-4000-8000-000000000010', now() - interval '15 minutes',
 'warning',
 'Checkpoints occurring too frequently -- consider increasing checkpoint_completion_target or max_wal_size',
 '[{"order":1,"signal":"log_checkpoint_too_frequent","description":"Checkpoints occurring too frequently","evidence":"checkpoints are occurring too frequently (8 seconds apart)"}]',
 NULL, ARRAY['log_checkpoint_too_frequent'],
 NULL, 'safe', 'log_deterministic', 0.85, 'production', 6),

-- 18. Lock timeout
('b0000001-0001-4000-8000-000000000011', now() - interval '9 minutes',
 'warning',
 'Lock wait timeout exceeded',
 '[{"order":1,"signal":"log_lock_timeout","description":"Lock wait timeout exceeded","evidence":"canceling statement due to lock timeout"}]',
 NULL, ARRAY['log_lock_timeout'],
 NULL, 'safe', 'log_deterministic', 0.85, 'production', 2),

-- 19. Statement timeout
('b0000001-0001-4000-8000-000000000012', now() - interval '6 minutes',
 'warning',
 'Statement timeout exceeded',
 '[{"order":1,"signal":"log_statement_timeout","description":"Statement timeout exceeded","evidence":"canceling statement due to statement timeout"}]',
 NULL, ARRAY['log_statement_timeout'],
 NULL, 'safe', 'log_deterministic', 0.85, 'analytics', 1),

-- 20. Replication conflict
('b0000001-0001-4000-8000-000000000013', now() - interval '28 minutes',
 'warning',
 'Replication conflict with recovery on standby',
 '[{"order":1,"signal":"log_replication_conflict","description":"Replication conflict with recovery on standby","evidence":"canceling statement due to conflict with recovery"}]',
 NULL, ARRAY['log_replication_conflict'],
 NULL, 'moderate', 'log_deterministic', 0.85, 'production', 1),

-- 21. WAL segment removed
('b0000001-0001-4000-8000-000000000014', now() - interval '50 minutes',
 'critical',
 'WAL segment removed before replica could consume it -- replica may need rebuild',
 '[{"order":1,"signal":"log_wal_segment_removed","description":"WAL segment removed before replica could consume it","evidence":"requested WAL segment 00000001000000030000001F has already been removed"}]',
 NULL, ARRAY['log_wal_segment_removed'],
 NULL, 'high_risk', 'log_deterministic', 0.85, 'production', 1),

-- 22. Autovacuum cancel
('b0000001-0001-4000-8000-000000000015', now() - interval '14 minutes',
 'warning',
 'Autovacuum task cancelled due to lock conflict',
 '[{"order":1,"signal":"log_autovacuum_cancel","description":"Autovacuum task cancelled due to lock conflict","evidence":"canceling autovacuum task on public.orders"}]',
 ARRAY['public.orders'], ARRAY['log_autovacuum_cancel'],
 NULL, 'safe', 'log_deterministic', 0.85, 'production', 3),

-- 23. Replication slot inactive
('b0000001-0001-4000-8000-000000000016', now() - interval '35 minutes',
 'warning',
 'Inactive replication slot accumulating WAL',
 '[{"order":1,"signal":"log_replication_slot_inactive","description":"Inactive replication slot accumulating WAL","evidence":"replication slot standby_slot is not active, retained WAL 12 GB"}]',
 NULL, ARRAY['log_replication_slot_inactive'],
 'SELECT slot_name, active, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained FROM pg_replication_slots WHERE NOT active;',
 'moderate', 'log_deterministic', 0.85, 'production', 1),

-- 24. Authentication failure
('b0000001-0001-4000-8000-000000000017', now() - interval '11 minutes',
 'warning',
 'Authentication failure',
 '[{"order":1,"signal":"log_authentication_failure","description":"Authentication failure","evidence":"password authentication failed for user app_readonly"}]',
 NULL, ARRAY['log_authentication_failure'],
 NULL, 'safe', 'log_deterministic', 0.85, 'production', 8),

-- 25. Slow query
('b0000001-0001-4000-8000-000000000018', now() - interval '4 minutes',
 'info',
 'Slow query detected via log_min_duration_statement',
 '[{"order":1,"signal":"log_slow_query","description":"Slow query detected via log_min_duration_statement","evidence":"duration=12500ms query=SELECT o.*, li.* FROM orders o JOIN line_items li ON li.order_id = o.id WHERE o.cre..."}]',
 ARRAY['analytics'], ARRAY['log_slow_query'],
 NULL, 'safe', 'log_deterministic', 0.85, 'analytics', 1)

ON CONFLICT (id) DO NOTHING;

-- ===========================================================================
-- Self-action correlation incidents
-- ===========================================================================

INSERT INTO sage.incidents (
    id, detected_at, severity, root_cause, causal_chain,
    affected_objects, signal_ids, recommended_sql, rollback_sql,
    action_risk, source, confidence, database_name, occurrence_count
) VALUES

-- 26. Self-caused: drop_index → slow query
('c0000001-0001-4000-8000-000000000001', now() - interval '16 minutes',
 'warning',
 'Self-caused: Dropping index may have caused query performance regression (action act-501: DROP INDEX idx_users_email)',
 '[{"order":1,"signal":"drop_index","description":"pg_sage executed drop_index","evidence":"Action act-501 at 2025-04-13T21:45:00Z: DROP INDEX idx_users_email"},{"order":2,"signal":"log_slow_query","description":"Dropping index may have caused query performance regression","evidence":"Incident detected at 2025-04-13T22:00:00Z"}]',
 ARRAY['public.users'], ARRAY['log_slow_query'],
 NULL, 'CREATE INDEX CONCURRENTLY idx_users_email ON users(email);',
 'high_risk', 'self_action', 0.90, 'production', 1),

-- 27. Self-caused: create_index → disk full
('c0000001-0001-4000-8000-000000000002', now() - interval '32 minutes',
 'critical',
 'Self-caused: Index creation consumed remaining disk space (action act-499: CREATE INDEX CONCURRENTLY idx_orders_large)',
 '[{"order":1,"signal":"create_index","description":"pg_sage executed create_index","evidence":"Action act-499 at 2025-04-13T21:30:00Z: CREATE INDEX CONCURRENTLY idx_orders_large"},{"order":2,"signal":"log_disk_full","description":"Index creation consumed remaining disk space","evidence":"Incident detected at 2025-04-13T21:45:00Z"}]',
 ARRAY['public.orders'], ARRAY['log_disk_full'],
 NULL, 'DROP INDEX CONCURRENTLY IF EXISTS idx_orders_large;',
 'high_risk', 'self_action', 0.90, 'production', 1),

-- 28. Manual review required: vacuum_full rolled back too many times
('c0000001-0001-4000-8000-000000000003', now() - interval '20 minutes',
 'critical',
 'Manual review required: vacuum_full has been rolled back 2+ times in 30 days. VACUUM FULL holds AccessExclusive lock, causing lock timeouts',
 '[{"order":1,"signal":"vacuum_full","description":"Repeated rollback detected (anti-oscillation)","evidence":"Action family vacuum_full exceeds rollback threshold of 2"}]',
 ARRAY['public.large_table'], ARRAY['log_lock_timeout'],
 NULL, NULL,
 'high_risk', 'manual_review_required', 1.00, 'production', 1)

ON CONFLICT (id) DO NOTHING;

-- ===========================================================================
-- TIER 2: LLM correlation incidents
-- ===========================================================================

INSERT INTO sage.incidents (
    id, detected_at, severity, root_cause, causal_chain,
    affected_objects, signal_ids, recommended_sql, action_risk,
    source, confidence, database_name, occurrence_count
) VALUES

-- 29. LLM-correlated: cascading failure
('d0000001-0001-4000-8000-000000000001', now() - interval '19 minutes',
 'critical',
 'Cascading failure: a bulk data import exhausted work_mem, spilling to disk (temp files), which filled the disk and triggered connection refusals as new backends could not allocate shared memory',
 '[{"order":1,"signal":"custom_bulk_import","description":"Bulk data import consuming excessive resources","evidence":"Detected via multiple concurrent signals"},{"order":2,"signal":"custom_memory_pressure","description":"Memory pressure from sort/hash operations spilling to disk","evidence":"temp_file_bytes > 1GB across 5 backends"},{"order":3,"signal":"custom_disk_pressure","description":"Disk space exhaustion from temp files and WAL accumulation","evidence":"Disk usage crossed 95% threshold"}]',
 ARRAY['pg_stat_activity','pg_stat_bgwriter'], ARRAY['custom_bulk_import','custom_memory_pressure','custom_disk_pressure'],
 'SET work_mem = ''256MB''; -- increase for bulk import sessions',
 'medium', 'llm', 0.60, 'production', 1),

-- 30. LLM-correlated: replication + vacuum interaction
('d0000001-0001-4000-8000-000000000002', now() - interval '42 minutes',
 'warning',
 'Replication lag caused by long-running autovacuum on primary holding back WAL replay on standby, compounded by inactive replication slot retaining excessive WAL',
 '[{"order":1,"signal":"custom_vacuum_lag","description":"Autovacuum running extended VACUUM on large table","evidence":"autovacuum: VACUUM public.events ran for 45 minutes"},{"order":2,"signal":"custom_repl_delay","description":"Standby WAL replay falling behind primary","evidence":"Replay lag: 120s, increasing trend"}]',
 ARRAY['pg_stat_replication','pg_replication_slots'], ARRAY['custom_vacuum_lag','custom_repl_delay'],
 'SELECT slot_name, active, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained FROM pg_replication_slots;',
 'low', 'llm', 0.60, 'production', 2)

ON CONFLICT (id) DO NOTHING;

-- ===========================================================================
-- One resolved incident (to show in the Resolved tab)
-- ===========================================================================

INSERT INTO sage.incidents (
    id, detected_at, severity, root_cause, causal_chain,
    affected_objects, signal_ids, recommended_sql, action_risk,
    source, confidence, resolved_at, database_name, occurrence_count
) VALUES
('e0000001-0001-4000-8000-000000000001', now() - interval '2 hours',
 'warning',
 'Autovacuum falling behind -- dead tuple accumulation',
 '[{"order":1,"signal":"vacuum_blocked","description":"No idle-in-tx blocker found; autovacuum workers cannot keep pace","evidence":"Tables with high dead tuples: [public.events, public.audit_log]"}]',
 ARRAY['public.events','public.audit_log'], ARRAY['vacuum_blocked'],
 NULL, 'safe', 'deterministic', 0.85,
 now() - interval '1 hour', 'production', 8)
ON CONFLICT (id) DO NOTHING;

-- ===========================================================================
-- One escalated incident
-- ===========================================================================

UPDATE sage.incidents
SET escalated_at = now() - interval '10 minutes'
WHERE id = 'b0000001-0001-4000-8000-000000000002'
  AND escalated_at IS NULL;
