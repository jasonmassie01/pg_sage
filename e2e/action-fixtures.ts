import { execFileSync } from 'child_process';
import { fixtureTargets } from './fixture-targets';

export function fixtureSQL(database: string, sql: string): string {
  if (process.env.PG_SAGE_E2E_FIXTURE !== 'full-surface') {
    throw new Error('Action fixtures require the disposable full-surface environment');
  }
  const target = fixtureTargets.find((item) => item.name === database);
  if (!target) throw new Error(`Unknown disposable fixture: ${database}`);
  return execFileSync('docker', [
    'exec', target.container, 'psql', '-v', 'ON_ERROR_STOP=1',
    '-U', 'postgres', '-d', target.db, '-At', '-c', sql,
  ], { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }).trim();
}

// These rows model a real analyzer recommendation awaiting explicit human approval.
// They never manufacture successful execution or automatic verification evidence.
export function seedAction(kind: 'approve' | 'reject' | 'events'): number {
  const database = kind === 'reject' ? 'health_test' : 'testdb';
  const sql = kind === 'approve'
    ? 'CREATE INDEX CONCURRENTLY e2e_workflow_idx ON public.e2e_workflow_orders (customer_id)'
    : 'DROP INDEX CONCURRENTLY public.e2e_workflow_reject_idx';
  fixtureSQL(database, `
    CREATE TABLE IF NOT EXISTS public.e2e_workflow_orders (id integer, customer_id integer);
    ${kind === 'approve' ? 'DROP INDEX IF EXISTS public.e2e_workflow_idx;' : ''}
    CREATE INDEX IF NOT EXISTS e2e_workflow_reject_idx
      ON public.e2e_workflow_orders (id);
    UPDATE sage.action_queue SET status = 'rejected'
      WHERE status = 'pending' AND identity_key = 'e2e_workflow_${kind}';
    UPDATE sage.findings SET status = 'resolved', resolved_at = now()
      WHERE status = 'open' AND object_identifier = 'e2e_workflow_${kind}';
  `);
  return Number(fixtureSQL(database, `
    WITH finding AS (
      INSERT INTO sage.findings
        (category, severity, title, detail, object_type, object_identifier, recommended_sql)
      VALUES ('e2e_workflow', 'info', 'Browser action fixture', '{}', 'table',
        'e2e_workflow_${kind}', $sql$${sql}$sql$) RETURNING id
    ) INSERT INTO sage.action_queue
      (finding_id, proposed_sql, action_risk, action_type, identity_key)
      SELECT id, $sql$${sql}$sql$, 'low',
        '${kind === 'approve' ? 'create_index' : 'drop_index'}', 'e2e_workflow_${kind}'
      FROM finding RETURNING id;
  `).split('\n')[0]);
}
