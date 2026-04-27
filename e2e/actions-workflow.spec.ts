import { test, expect, Page } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

type PendingAction = {
  id: number;
  database_name: string;
  finding_id: number;
  proposed_sql: string;
  status: string;
};

type Finding = {
  id: number | string;
  category: string;
  title?: string;
  object_identifier?: string;
  detail?: unknown;
  action_log_id?: number | string | null;
};

type ActionLogEntry = {
  id: number | string;
  finding_id?: number | string | null;
  outcome?: string;
  sql_executed?: string;
};

async function fetchPending(page: Page): Promise<PendingAction[]> {
  return page.evaluate(async () => {
    const res = await fetch('/api/v1/actions/pending', {
      credentials: 'include',
    });
    if (!res.ok) {
      throw new Error(`pending actions failed: ${res.status}`);
    }
    const json = await res.json();
    return json.pending;
  });
}

async function rowForAction(page: Page, action: PendingAction) {
  const row = page.locator('tbody tr')
    .filter({
      has: page.locator(`td:text-is("${action.database_name}")`),
    })
    .filter({
      has: page.locator(`td:text-is("${action.finding_id}")`),
    });
  await expect(row).toHaveCount(1);
  return row;
}

async function fetchFindings(
  page: Page,
  database: string,
  status: 'open' | 'resolved',
): Promise<Finding[]> {
  return page.evaluate(async ({ database, status }) => {
    const res = await fetch(
      `/api/v1/findings?status=${status}&database=${
        encodeURIComponent(database)
      }&limit=200`,
      { credentials: 'include' },
    );
    if (!res.ok) {
      throw new Error(`findings ${status} failed: ${res.status}`);
    }
    const json = await res.json();
    return json.findings || [];
  }, { database, status });
}

test.describe.serial('Actions workflow', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
  });

  test.afterEach(async () => {
    expect(consoleErrors).toEqual([]);
  });

  test('rejects a pending action from the UI and removes it from pending', async ({
    page,
  }) => {
    const pending = await fetchPending(page);
    const target = pending.find((a) =>
      a.database_name === 'health_test' &&
      a.proposed_sql.startsWith('DROP INDEX CONCURRENTLY'),
    );
    expect(target, 'expected a rejectable health_test action').toBeTruthy();

    await page.goto('/#/actions');
    await page.getByTestId('actions-tab-pending').click();

    const row = await rowForAction(page, target!);
    await row.getByTestId('reject-button').click();

    const reason = page.getByPlaceholder('Reason for rejection...');
    await expect(reason).toBeVisible();
    await reason.fill('e2e reject workflow');

    const rejectResponse = page.waitForResponse((res) =>
      res.url().includes(`/api/v1/actions/${target!.id}/reject`) &&
      res.status() === 200,
    );
    await page.getByRole('button', { name: 'Confirm Reject' }).click();
    const rejectJson = await (await rejectResponse).json();
    expect(rejectJson.ok).toBe(true);
    expect(rejectJson.database).toBe('health_test');

    await expect(page.locator('main').getByText(
      `Action ${target!.id} rejected`,
    )).toBeVisible();

    const after = await fetchPending(page);
    expect(after.some((a) => a.id === target!.id &&
      a.database_name === target!.database_name)).toBe(false);
  });

  test('approves a pending create-index action and keeps it resolved after refresh', async ({
    page,
  }) => {
    const pending = await fetchPending(page);
    const target = pending.find((a) =>
      a.database_name === 'testdb' &&
      a.proposed_sql.includes(
        'CREATE INDEX CONCURRENTLY ON public.orders',
      ),
    );
    test.skip(!target, 'requires the testdb public.orders action');

    await page.goto('/#/actions');
    await page.getByTestId('actions-tab-pending').click();

    const row = await rowForAction(page, target!);
    const approveResponse = page.waitForResponse((res) =>
      res.url().includes(`/api/v1/actions/${target!.id}/approve`) &&
      res.status() === 200,
    );
    await row.getByTestId('approve-button').click();
    const approveJson = await (await approveResponse).json();

    expect(approveJson.ok, JSON.stringify(approveJson)).toBe(true);
    expect(approveJson.executed).toBe(true);
    expect(approveJson.database).toBe(target!.database_name);
    expect(approveJson.action_log_id).toBeGreaterThan(0);

    await expect(page.locator('main').getByText(
      `Action ${target!.id} approved and executed`,
    )).toBeVisible();

    const after = await fetchPending(page);
    expect(after.some((a) => a.id === target!.id &&
      a.database_name === target!.database_name)).toBe(false);

    const actionLog = await page.evaluate(async ({ database, actionLogID }) => {
      const res = await fetch(
        `/api/v1/actions?database=${encodeURIComponent(database)}&limit=100`,
        { credentials: 'include' },
      );
      if (!res.ok) {
        throw new Error(`actions log failed: ${res.status}`);
      }
      const json = await res.json();
      return (json.actions || []).find((a) =>
        Number(a.id) === Number(actionLogID)
      ) || null;
    }, {
      database: target!.database_name,
      actionLogID: approveJson.action_log_id,
    }) as ActionLogEntry | null;

    expect(actionLog, 'approved action should be visible in executed log')
      .toBeTruthy();
    expect(Number(actionLog!.finding_id)).toBe(target!.finding_id);
    expect(actionLog!.outcome).toBe('success');
    expect(actionLog!.sql_executed).toContain(
      'CREATE INDEX CONCURRENTLY ON public.orders',
    );

    const findingState = await page.evaluate(async (target) => {
      const [openRes, resolvedRes] = await Promise.all([
        fetch(
          `/api/v1/findings?status=open&database=${
            encodeURIComponent(target.database_name)
          }&limit=100`,
          { credentials: 'include' },
        ),
        fetch(
          `/api/v1/findings?status=resolved&database=${
            encodeURIComponent(target.database_name)
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
          .find((f) => Number(f.id) === Number(target.finding_id)) || null,
        resolved: (resolvedJson.findings || [])
          .find((f) => Number(f.id) === Number(target.finding_id)) || null,
      };
    }, target);

    expect(findingState.open).toBeNull();
    expect(findingState.resolved).toBeTruthy();
    expect(Number(findingState.resolved.action_log_id))
      .toBe(approveJson.action_log_id);

    await page.waitForTimeout(12_000);

    const refreshedOpen = await fetchFindings(
      page,
      target!.database_name,
      'open',
    );
    expect(
      refreshedOpen.some((f) => Number(f.id) === target!.finding_id),
      'approved action finding must not reopen after refresh',
    ).toBe(false);

    const refreshedResolved = await fetchFindings(
      page,
      target!.database_name,
      'resolved',
    );
    const resolvedFinding = refreshedResolved.find((f) =>
      Number(f.id) === target!.finding_id
    );
    expect(resolvedFinding).toBeTruthy();
    expect(Number(resolvedFinding!.action_log_id))
      .toBe(approveJson.action_log_id);
  });
});
