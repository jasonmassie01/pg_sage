import { test, expect, Page } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

type InlineTarget = {
  queueId: number;
  findingId: number;
  database: string;
  title: string;
};

async function findInlineTarget(
  page: Page,
  predicate: (sql: string, database: string) => boolean,
): Promise<InlineTarget | null> {
  return page.evaluate(async ({ predicateText }) => {
    const predicate = new Function(
      'sql',
      'database',
      `return (${predicateText})(sql, database);`,
    );
    const pendingRes = await fetch('/api/v1/actions/pending', {
      credentials: 'include',
    });
    const pending = (await pendingRes.json()).pending || [];
    const byFinding = pending.reduce((acc, a) => {
      const key = `${a.database_name}:${a.finding_id}`;
      acc[key] = (acc[key] || 0) + 1;
      return acc;
    }, {});
    for (const action of pending) {
      if (!predicate(action.proposed_sql, action.database_name)) continue;
      if (byFinding[`${action.database_name}:${action.finding_id}`] !== 1) {
        continue;
      }
      const findingsRes = await fetch(
        `/api/v1/findings?status=open&database=${
          encodeURIComponent(action.database_name)
        }&limit=100`,
        { credentials: 'include' },
      );
      const findings = (await findingsRes.json()).findings || [];
      const finding = findings.find((f) =>
        Number(f.id) === Number(action.finding_id)
      );
      if (!finding) continue;
      return {
        queueId: Number(action.id),
        findingId: Number(action.finding_id),
        database: action.database_name,
        title: finding.title,
      };
    }
    return null;
  }, { predicateText: predicate.toString() });
}

async function openFindingDetail(page: Page, target: InlineTarget) {
  await page.goto('/#/findings');
  await page.getByTestId('database-picker').selectOption(target.database);
  const row = page.locator(`tbody tr[data-row-key="${target.findingId}"]`);
  await expect(row).toHaveCount(1);
  await row.click();
  await expect(page.getByTestId('pending-action-panel')).toBeVisible();
}

test.describe.serial('Findings inline actions', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('rejects a pending action inline from recommendation detail', async ({
    page,
  }) => {
    const target = await findInlineTarget(page, (sql, database) =>
      database === 'testdb2' &&
      sql.startsWith('DROP INDEX CONCURRENTLY'),
    );
    test.skip(!target, 'requires a rejectable testdb2 pending action');

    await openFindingDetail(page, target!);

    page.once('dialog', async (dialog) => {
      expect(dialog.message()).toContain('Reject reason');
      await dialog.accept('e2e inline reject');
    });

    const rejectResponse = page.waitForResponse((res) =>
      res.url().includes(`/api/v1/actions/${target!.queueId}/reject`) &&
      res.status() === 200,
    );
    await page.getByTestId(`reject-pending-${target!.queueId}`).click();
    const json = await (await rejectResponse).json();
    expect(json.ok).toBe(true);
    expect(json.database).toBe(target!.database);

    await expect(page.getByTestId('pending-action-panel')).toHaveCount(0);
    const stillPending = await page.evaluate(async (target) => {
      const res = await fetch('/api/v1/actions/pending', {
        credentials: 'include',
      });
      const pending = (await res.json()).pending || [];
      return pending.some((a) =>
        Number(a.id) === target.queueId &&
        a.database_name === target.database
      );
    }, target);
    expect(stillPending).toBe(false);
  });

  test('approves and executes a pending create-index action inline', async ({
    page,
  }) => {
    const target = await findInlineTarget(page, (sql, database) =>
      database === 'health_test' &&
      sql.includes('CREATE INDEX CONCURRENTLY ON sage_verify.orders'),
    );
    test.skip(!target, 'requires the health_test orders create-index action');

    await openFindingDetail(page, target!);

    const approveResponse = page.waitForResponse((res) =>
      res.url().includes(`/api/v1/actions/${target!.queueId}/approve`) &&
      res.status() === 200,
    );
    await page.getByTestId(`approve-pending-${target!.queueId}`).click();
    const json = await (await approveResponse).json();
    expect(json.ok, JSON.stringify(json)).toBe(true);
    expect(json.executed).toBe(true);
    expect(json.database).toBe(target!.database);
    expect(json.action_log_id).toBeGreaterThan(0);

    const findingState = await page.evaluate(async (target) => {
      const [openRes, resolvedRes] = await Promise.all([
        fetch(
          `/api/v1/findings?status=open&database=${
            encodeURIComponent(target.database)
          }&limit=100`,
          { credentials: 'include' },
        ),
        fetch(
          `/api/v1/findings?status=resolved&database=${
            encodeURIComponent(target.database)
          }&limit=100`,
          { credentials: 'include' },
        ),
      ]);
      const [openJson, resolvedJson] = await Promise.all([
        openRes.json(),
        resolvedRes.json(),
      ]);
      return {
        open: (openJson.findings || [])
          .find((f) => Number(f.id) === target.findingId) || null,
        resolved: (resolvedJson.findings || [])
          .find((f) => Number(f.id) === target.findingId) || null,
      };
    }, target);
    expect(findingState.open).toBeNull();
    expect(findingState.resolved).toBeTruthy();
    expect(Number(findingState.resolved.action_log_id))
      .toBe(json.action_log_id);
  });
});
