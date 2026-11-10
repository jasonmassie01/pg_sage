package schema

// ddlTrustLedgerIncidentCursors adds the incident reconciler's cursors to
// sage.trust_ledger_state (earned-02): per kind (L2 handoffs, L3
// auto-executions) the largest action_log id read past the 5-minute lag,
// and the ids at or below it still waiting for a verdict or a notice.
// NULL means "read the whole 30-day lookback", so the first pass after
// the upgrade reads what the pass before it read. Control database only;
// one row per database. The catalog check first means a re-run takes no
// lock on the table; lock_timeout bounds the wait of the first run.
const ddlTrustLedgerIncidentCursors = `
SET LOCAL lock_timeout = '5s';
DO $incident_cursors$
BEGIN
    IF (SELECT count(*) FROM pg_attribute
         WHERE attrelid = 'sage.trust_ledger_state'::regclass
           AND attname IN ('handoff_cursor', 'handoff_pending', 'auto_cursor',
                           'auto_pending')
           AND NOT attisdropped) < 4 THEN
        ALTER TABLE sage.trust_ledger_state
            ADD COLUMN IF NOT EXISTS handoff_cursor bigint,
            ADD COLUMN IF NOT EXISTS handoff_pending bigint[],
            ADD COLUMN IF NOT EXISTS auto_cursor bigint,
            ADD COLUMN IF NOT EXISTS auto_pending bigint[];
    END IF;
END
$incident_cursors$;
`
