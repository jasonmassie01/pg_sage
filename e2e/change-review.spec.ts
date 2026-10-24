import { execFileSync } from 'child_process';
import { expect, test, type APIRequestContext } from '@playwright/test';
import { login } from './helpers';

// CHECK-UI-05: Change Review end to end on a disposable pg_sage server with
// llm.enabled false. A CI-style client submits a review over the MCP
// endpoint with a seeded token; the admin opens #/reviews from the
// "Reviews" nav item, records Concur and sees credit "pending". After the
// test marks the receipt clean in SQL (the receipt job's transition), the
// Value page lists "Change Review".
//
// Environment (never a monitored user database):
//   PG_SAGE_E2E_BASE_URL       the server, e.g. http://localhost:8085
//   PG_SAGE_ADMIN_EMAIL/PASS   an ephemeral admin
//   PG_SAGE_E2E_MCP_TOKEN      an MCP token with read and propose scope bound to
//                              PG_SAGE_E2E_CR_DATABASE
//   PG_SAGE_E2E_CR_DATABASE    the database name pg_sage knows
//   PG_SAGE_E2E_CR_CONTAINER   the disposable Postgres container
//   PG_SAGE_E2E_CR_DB          that database's physical name in the container

const env = {
  base: process.env.PG_SAGE_E2E_BASE_URL || '',
  email: process.env.PG_SAGE_ADMIN_EMAIL || '',
  pass: process.env.PG_SAGE_ADMIN_PASS || '',
  token: process.env.PG_SAGE_E2E_MCP_TOKEN || '',
  database: process.env.PG_SAGE_E2E_CR_DATABASE || '',
  container: process.env.PG_SAGE_E2E_CR_CONTAINER || '',
  db: process.env.PG_SAGE_E2E_CR_DB || '',
};
const missing = Object.entries(env).filter(([, v]) => !v).map(([k]) => k);

function sql(statement: string): string {
  return execFileSync('docker', [
    'exec', env.container, 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres',
    '-d', env.db, '-At', '-c', statement,
  ], { encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }).trim();
}

async function mcp(request: APIRequestContext, id: number, method: string,
  params: Record<string, unknown>): Promise<Record<string, unknown>> {
  const res = await request.post('/api/v1/mcp', {
    headers: {
      Authorization: `Bearer ${env.token}`,
      Accept: 'application/json, text/event-stream',
      'MCP-Protocol-Version': '2025-06-18',
    },
    data: { jsonrpc: '2.0', id, method, params },
  });
  expect(res.status(), `${method} status`).toBe(200);
  const body = await res.json();
  expect(body.error, `${method} error`).toBeUndefined();
  return body.result;
}

async function submitReview(request: APIRequestContext): Promise<string> {
  await mcp(request, 1, 'initialize', { protocolVersion: '2025-06-18', capabilities: {},
    clientInfo: { name: 'e2e-change-review', version: '1' } });
  const run = Date.now();
  const result = await mcp(request, 2, 'tools/call', { name: 'review_change',
    arguments: {
      database: env.database,
      sql: `ALTER TABLE public.e2e_cr_orders ALTER COLUMN email TYPE varchar(20); -- ${run}`,
      pr: { url: `https://github.com/acme/e2e/pull/${run % 100000 + 1}`,
        title: `E2E change review ${run}`, repo: 'acme/e2e' },
      wait_seconds: 50,
    } }) as { isError?: boolean; structuredContent?: Record<string, unknown> };
  expect(result.isError).toBeFalsy();
  const review = result.structuredContent || {};
  expect(review.state).toBe('complete');
  expect(review.model_status).toBe('no_model');
  expect(['review', 'risky']).toContain(review.verdict);
  // The id is interpolated into the fixture SQL below: only a UUID.
  expect(String(review.id)).toMatch(/^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$/);
  return String(review.id);
}

test.describe('Change Review', () => {
  test.skip(missing.length > 0, `requires ${missing.join(', ')} (see header)`);
  test.describe.configure({ mode: 'serial' });

  test('submit over MCP, concur in the UI, credit lands on the Value page', async ({
    page, playwright,
  }) => {
    sql(`CREATE TABLE IF NOT EXISTS public.e2e_cr_orders (id bigint PRIMARY KEY,
      email text)`);
    const request = await playwright.request.newContext({ baseURL: env.base });
    const id = await submitReview(request);
    await request.dispose();

    await login(page, env.email, env.pass);
    await page.getByTestId('nav-reviews').click();
    await expect(page.getByTestId('reviews-page')).toBeVisible();
    const row = page.getByTestId('review-row').filter({ hasText: 'E2E change review' })
      .first();
    await expect(row.getByTestId('review-dba-verdict')).toHaveText('Awaiting DBA');
    await row.getByRole('link').first().click();

    await expect(page.getByTestId('review-detail')).toBeVisible();
    await expect(page.getByTestId('review-model-status')).toContainText('no model');
    await page.getByTestId('verdict-concur').click();
    await page.getByTestId('verdict-submit').click();
    await expect(page.getByTestId('verdict-decided')).toContainText('Concur');
    await expect(page.getByTestId('review-credit')).toContainText('pending');

    // The receipt job's transition, done directly: landed and clean, credit
    // final (credit.go: concur + clean receipt).
    sql(`UPDATE sage.change_review SET landing_state = 'landed',
      landing_source = 'reported', landed_lower = now(), landed_at = now(),
      receipt_verdict = 'clean', receipt_decided_at = now(), credit_state = 'final',
      credit_final_at = now() WHERE id = '${id}'`);
    await page.getByTestId('nav-value').click();
    const features = page.getByRole('table', { name: /hours saved by feature/i });
    await expect(features.getByRole('rowheader', { name: 'Change Review' }))
      .toBeVisible({ timeout: 15000 });
  });
});
