import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

async function postExplain(page: any, body: Record<string, unknown>) {
  const res = await page.request.post('/api/v1/explain', {
    data: body,
  });
  return { status: res.status(), json: await res.json() };
}

test.describe('Explain API', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('explains a safe query and rejects invalid requests', async ({
    page,
  }) => {
    const ok = await postExplain(page, {
      query: 'SELECT 1',
      plan_only: true,
    });
    test.skip(ok.status === 503, 'Explain endpoint disabled locally');

    expect(ok.status).toBe(200);
    expect(ok.json.query).toBe('SELECT 1');
    expect(ok.json.plan_json).toBeTruthy();
    expect(Array.isArray(ok.json.node_breakdown)).toBeTruthy();

    const empty = await postExplain(page, {});
    expect(empty.status).toBe(400);
    expect(empty.json.error).toContain('query or query_id is required');

    const ddl = await postExplain(page, {
      query: 'CREATE TABLE codex_explain_nope(id int)',
    });
    expect(ddl.status).toBe(400);
    expect(ddl.json.error).toContain('DDL/admin statements');

    const queryID = await postExplain(page, { query_id: 12345 });
    expect(queryID.status).toBe(400);
    expect(queryID.json.error).toContain('not yet implemented');
  });

  test('rejects malformed database selectors at the boundary', async ({
    page,
  }) => {
    const res = await page.request.post(
      '/api/v1/explain?database=bad/name',
      {
        data: { query: 'SELECT 1', plan_only: true },
      },
    );
    const result = {
      status: res.status(),
      json: await res.json(),
    };

    expect(result.status).toBe(400);
    expect(result.json.error).toContain('invalid database parameter');
  });
});
