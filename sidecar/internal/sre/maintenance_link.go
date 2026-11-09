package sre

// MaintenanceActionLink identifies the exact durable executor record. An
// execution can commit before the bridge saves its action ID; preserve that
// crash window only for an unresolved proposal and the exact bound finding,
// forward SQL and inverse. Once an ID is recorded, no replacement action can
// claim its lifetime. The finding expression index bounds the JSON lookup.
// Retention holds and the maintenance reconciler share it; p aliases
// sage.sre_maintenance_proposals and al aliases sage.action_log.
const MaintenanceActionLink = `p.proposal->>'finding_id' = al.finding_id::text
    AND (p.proposal->>'action_log_id' = al.id::text
         OR (p.state IN ('executing', 'uncertain')
             AND COALESCE(p.proposal->>'action_log_id', '0') = '0'
             AND p.proposal->>'sql' = al.sql_executed
             AND p.proposal->>'rollback_sql' = al.rollback_sql
             AND EXISTS (SELECT 1 FROM sage.findings f
                 WHERE f.id = al.finding_id AND f.category = 'sre_maintenance'
                   AND f.object_identifier = 'sre_maintenance:' || p.id::text
                   AND f.recommended_sql = al.sql_executed
                   AND f.rollback_sql = al.rollback_sql)))`
