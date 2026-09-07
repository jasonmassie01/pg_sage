import { test, expect } from '@playwright/test';
import { getConsoleErrors } from './helpers';
import { fixtureTargets } from './fixture-targets';
import {
  adminEmail, adminPassword, apiLogin, body, databaseInput, databaseLifecycle, get,
  localSink, login, managed, notificationLifecycle, object, open, path, post, put,
  remove, rows, secondTarget, seedQueryWorkload, target, unique, withConfig, withUser,
} from './walkthrough-support';

// Scenario consolidation/mapping: tasks/walkthrough-repair-2026-09-04.md.
// Run only against disposable fixture targets, with --workers=1 and no other
// mutable suites. Independent tests restore their own changes in finally blocks.
// Browser/API suites exercise real services; no mocked API responses or outbound providers.
test.setTimeout(60_000);
test.beforeEach(async ({ request }, info) => {
  expect(adminPassword, 'Missing explicit admin setup').not.toBe('');
  if (!info.title.startsWith('AUTH:')) await apiLogin(request);
});

test('AUTH: CHECK-01/03/46 invalid login retains form and error', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('#root')).toBeVisible();
  await expect(page.getByTestId('login-email')).toBeVisible();
  await page.getByTestId('login-email').fill(adminEmail);
  await page.getByTestId('login-password').fill('known-invalid-audit-password');
  await page.getByTestId('login-submit').click();
  await expect(page.getByTestId('login-error')).toBeVisible();
  await expect(page.getByTestId('login-submit')).toBeVisible();
});

test('AUTH: CHECK-02/04/34/86/87 login and logout revoke session', async ({ page, request }) => {
  const user = await apiLogin(request);
  expect(user.role).toBe('admin');
  expect(Number(user.id)).toBeGreaterThan(0);
  expect((await get(request, '/auth/me')).email).toBe(adminEmail);
  await post(request, '/auth/logout');
  expect((await request.get(path('/auth/me'))).status()).toBe(401);
  await login(page);
  await expect(page.getByTestId('value-page')).toBeVisible();
  await page.getByTestId('sign-out-button').click();
  await expect(page.getByTestId('login-submit')).toBeVisible();
  await login(page);
});

test('AUTH: CHECK-107/110 missing login fields and unauthenticated APIs fail',
  async ({ request }) => {
  expect((await request.post(path('/auth/login'), { data: { email: '' } })).status()).toBe(400);
  expect((await request.get(path('/databases'))).status()).toBe(401);
});

test('CHECK-07/08/10/90 fixture fleet identity and active context agree',
  async ({ request, page }) => {
  const active = await get(request, '/databases');
  const configured = rows((await get(request, '/databases/managed')).databases);
  const expected = fixtureTargets.map(db => db.name).sort();
  expect(rows(active.databases).map(db => db.name).sort()).toEqual(expected);
  expect(configured.map(db => db.name).sort()).toEqual(expected);
  expect(object(active.summary).total_databases).toBe(expected.length);
  await login(page);
  await open(page, 'manage-databases', 'databases-table');
  for (const name of expected) await expect(page.getByTestId('db-row')
    .filter({ has: page.getByRole('cell', { name, exact: true }) }))
    .toBeVisible();
});

test('CHECK-05/06/83/84/85/91/112/114/120 managed database UI CRUD and real connection',
  async ({ request, page }) => {
    await login(page);
    await databaseLifecycle(page, request);
  });

test('CHECK-09/47/48/49/50/51 Value and advanced metrics render without JS errors',
  async ({ page }) => {
    const errors = getConsoleErrors(page);
    await login(page);
    await expect(page.getByTestId('value-page')).toBeVisible();
    await open(page, 'advanced', 'health-hero');
    await expect(page.getByTestId('stat-databases')).toContainText(String(fixtureTargets.length));
    await expect(page.getByTestId('db-list')).toContainText(target);
    await page.getByTestId('overview-tab-recent-recos').click();
    await expect(page.getByTestId('recent-findings')).toBeVisible();
    expect(errors).toEqual([]);
  });

for (const [route, label, source] of [
  ['cases', 'All', 'all'], ['forecasts', 'Forecasts', 'forecast'],
  ['query-hints', 'Query Hints', 'query_hint'], ['schema-health', 'Schema', 'schema_health'],
  ['incidents', 'Incidents', 'incident'],
]) {
  test(`CHECK-11/15/41/42/77/78 Cases route ${route} selects ${source}`,
  async ({ page, request }) => {
    const all = rows((await get(request, '/cases')).cases);
    const expected = source === 'all' ? all : all.filter(c => c.source_type === source);
    await login(page);
    await open(page, route, 'cases-page');
    await expect(page.getByRole('button', { name: label, exact: true }))
      .toHaveAttribute('aria-pressed', 'true');
    await expect(page.getByTestId('cases-page').locator('article')).toHaveCount(expected.length);
    await expect(page.getByTestId('cases-page-description'))
      .toContainText(`${expected.length} of ${all.length} cases are visible`);
  });
}

test('CHECK-12/13/14/65/66/67 case evidence, SQL and finding identity agree',
  async ({ request, page }) => {
  const findings = rows((await get(request, `/findings?database=${target}`)).findings);
  expect(findings.length).toBeGreaterThan(0);
  expect(findings.every(f => f.database_name === target)).toBe(true);
  const other = rows((await get(request, `/findings?database=${secondTarget}`)).findings);
  expect(other.every(f => f.database_name === secondTarget)).toBe(true);
  const detail = await get(request, `/findings/${findings[0].id}?database=${target}`);
  expect(detail.id).toBe(findings[0].id);
  expect(detail.title).toBe(findings[0].title);
  expect(String(detail.category)).not.toBe('');
  const cases = rows((await get(request, `/cases?database=${target}`)).cases);
  const findingCase = cases.find(c => c.source_type === 'finding');
  expect(findingCase).toBeDefined();
  expect(rows(findingCase!.evidence).length).toBeGreaterThan(0);
  expect(rows(findingCase!.action_candidates).length).toBeGreaterThan(0);
  await login(page);
  await page.getByTestId('database-picker').selectOption(target);
  await open(page, 'cases', 'cases-page');
  await page.getByRole('button', { name: 'Findings', exact: true }).click();
  await expect(page.getByLabel('Case evidence').first()).toBeVisible();
  await expect(page.getByTestId('cases-page')).toContainText('Migration script');
  await expect(page.getByTestId('cases-page')).toContainText(String(detail.recommended_sql));
});

test('CHECK-16/17/18 suppression changes persisted state and restores it', async ({ request }) => {
  const finding = rows((await get(request, `/findings?database=${target}`)).findings)[0];
  expect(finding).toBeDefined();
  const route = `/findings/${finding.id}`;
  let suppressed = false;
  try {
    await post(request, `${route}/suppress?database=${target}`);
    suppressed = true;
    expect((await get(request, `${route}?database=${target}`)).status).toBe('suppressed');
    await post(request, `${route}/unsuppress?database=${target}`);
    suppressed = false;
    expect((await get(request, `${route}?database=${target}`)).status).toBe('open');
  } finally {
    if (suppressed) await post(request, `${route}/unsuppress?database=${target}`);
    expect((await get(request, `${route}?database=${target}`)).status).toBe('open');
  }
});

test('CHECK-19/20/24 settings simple and advanced tabs expose scoped fields', async ({ page }) => {
  await login(page);
  await open(page, 'settings', 'settings-tab-monitoring');
  await page.getByTestId('settings-tab-monitoring').click();
  await expect(page.getByTestId('setting-analyzer.slow_query_threshold_ms')).toBeVisible();
  await page.getByTestId('settings-tab-ai-alerts').click();
  await expect(page.getByTestId('setting-llm.enabled')).toBeVisible();
  await page.getByTestId('settings-mode-toggle').click();
  await expect(page.getByTestId('settings-mode-toggle')).toHaveText('Show Simple');
  await expect(page.getByTestId('settings-tab-analyzer')).toBeVisible();
});

test('CHECK-21/22/70/71/72/102/105 settings save and discard preserve original config',
  async ({ page, request }) => {
    const key = 'analyzer.slow_query_threshold_ms';
    await withConfig(request, '/config/global', { [key]: 2000 }, async () => {
      await login(page);
      await open(page, 'settings', 'settings-tab-monitoring');
      await page.getByTestId('settings-tab-monitoring').click();
      const input = page.getByTestId(`setting-${key}`);
      await expect(input).toHaveValue('2000');
      await input.fill('2500');
      await page.getByTestId('settings-discard').click();
      await expect(input).toHaveValue('2000');
      await input.fill('3000');
      await page.getByTestId('settings-save').click();
      await page.getByTestId('config-diff-confirm').click();
      await expect.poll(async () => object(object((await get(request, '/config/global'))
        .config)[key]).value).toBe(3000);
      expect(rows((await get(request, '/config/audit')).audit).length).toBeGreaterThan(0);
    });
  });

test('CHECK-23/115 local LLM model discovery uses configured endpoint', async ({ request }) => {
  const sink = await localSink();
  try {
    await withConfig(request, '/config/global', {
      'llm.enabled': true, 'llm.endpoint': `${sink.url}/v1`, 'llm.model': 'local-audit-model',
      'llm.api_key': 'local-audit-key',
    }, async () => {
      const models = await get(request, '/llm/models');
      expect(JSON.stringify(models.models)).toContain('local-audit-model');
      expect(sink.received.some(v => String(v.url).endsWith('/models'))).toBe(true);
    });
  } finally { await sink.close(); }
});

test('CHECK-25/26/27/28/96/97/98/99/100/101 notification CRUD delivers only to local sink',
  async ({ page, request }) => {
    await login(page);
    await notificationLifecycle(page, request);
  });

test('CHECK-29/30/35 user UI creation owns and removes only its generated user',
  async ({ page, request }) => {
  const email = `${unique()}@test.local`;
  let id: unknown;
  try {
    await login(page);
    await open(page, 'users', 'users-table');
    await expect(page.getByTestId('users-table')).toContainText(adminEmail);
    await page.getByTestId('add-user-email').fill(email);
    await page.getByTestId('add-user-password').fill(`${unique()}!`);
    await page.getByTestId('add-user-role').selectOption('viewer');
    await page.getByTestId('add-user-submit').click();
    await expect(page.getByTestId('users-table')).toContainText(email);
    id = rows((await get(request, '/users')).users).find(u => u.email === email)?.id;
    expect(id).toBeDefined();
  } finally { if (id !== undefined) await remove(request, `/users/${id}`); }
  expect(rows((await get(request, '/users')).users).some(u => u.email === email)).toBe(false);
});

for (const role of ['viewer', 'operator']) {
  test(`CHECK-31/32/33/89 ${role} cannot access admin resources`, async ({ request, page }) => {
    await withUser(request, role, async (user, password) => {
      await apiLogin(request, String(user.email), password);
      for (const route of ['/users', '/config/global', '/databases/managed'])
        expect((await request.get(path(route))).status()).toBe(403);
      expect((await request.get(path('/actions/pending'))).status())
        .toBe(role === 'viewer' ? 403 : 200);
      await login(page, String(user.email), password);
      await expect(page.getByTestId('nav-settings')).toHaveCount(0);
      await expect(page.getByTestId('nav-databases')).toHaveCount(0);
      await open(page, 'settings', 'access-denied');
    });
  });
}

test('CHECK-88 role update persists and a new login observes it', async ({ request }) => {
  await withUser(request, 'viewer', async (user, password) => {
    await put(request, `/users/${user.id}/role`, { role: 'operator' });
    expect(rows((await get(request, '/users')).users).find(u => u.id === user.id)?.role)
      .toBe('operator');
    expect((await apiLogin(request, String(user.email), password)).role).toBe('operator');
  });
});

for (const scope of ['', `?database=${target}`]) {
  test(`CHECK-36/37/38/39/75/76 emergency stop and resume ${scope || 'fleet'}`,
    async ({ request, page }) => {
      expect(object((await get(request, '/databases')).summary).emergency_stopped).toBe(false);
      try {
        const stopped = await post(request, `/emergency-stop${scope}`);
        expect(stopped.status).toBe('stopped');
        expect(stopped.stopped).toBe(scope ? 1 : fixtureTargets.length);
        expect(object((await get(request, '/databases')).summary).emergency_stopped).toBe(true);
        await login(page);
        await expect(page.getByTestId('emergency-stop-badge')).toBeVisible();
        await open(page, 'settings', 'resume-button');
      } finally {
        expect((await post(request, `/resume${scope}`)).status).toBe('resumed');
        expect(object((await get(request, '/databases')).summary).emergency_stopped).toBe(false);
      }
    });
}

test('CHECK-40/54/55/56/57/92/117 actions tabs and pagination match API',
  async ({ request, page }) => {
  const actions = await get(request, `/actions?database=${target}&limit=5&offset=0`);
  expect(actions.database).toBe(target);
  expect(actions.limit).toBe(5);
  expect(actions.offset).toBe(0);
  expect(rows(actions.actions).length).toBeLessThanOrEqual(5);
  expect(Number(actions.total)).toBeGreaterThanOrEqual(rows(actions.actions).length);
  expect(Number((await get(request, '/actions/pending/count')).count)).toBeGreaterThanOrEqual(0);
  await login(page);
  await open(page, 'actions', 'actions-tab-pending');
  await page.getByTestId('actions-tab-pending').click();
  await expect(page.getByTestId('actions-tab-pending')).toBeVisible();
  await page.getByTestId('actions-tab-executed').click();
  await expect(page.getByTestId('actions-page-description')).toContainText('Actions');
});

test('CHECK-43/79 alerts expose date filters and alert records', async ({ request, page }) => {
  const alerts = await get(request, `/alert-log?database=${target}`);
  expect(alerts.database).toBe(target);
  rows(alerts.alerts);
  await login(page);
  await page.goto('/#/alerts');
  if (rows(alerts.alerts).length === 0) {
    await expect(page.getByText('No alerts sent yet.', { exact: false })).toBeVisible();
  } else {
    await expect(page.getByTestId('alert-log-date-from')).toBeVisible();
    await expect(page.getByTestId('alert-log-date-to')).toBeVisible();
  }
});

test('CHECK-44/45/73/74 metrics expose actual fleet and per-database identity',
  async ({ request }) => {
  const metricsUrl = process.env.PG_SAGE_E2E_METRICS_URL ?? 'http://127.0.0.1:9189/metrics';
  const metrics = await request.get(metricsUrl);
  expect(metrics.status()).toBe(200);
  expect(await metrics.text()).toMatch(/^pg_sage_/m);
  expect(object((await get(request, '/metrics')).fleet).total_databases)
    .toBe(fixtureTargets.length);
  const perDatabase = await get(request, `/metrics?database=${target}`);
  expect(perDatabase.database).toBe(target);
  expect(object(perDatabase.status).connected).toBe(true);
});

for (const metric of ['tables', 'indexes', 'queries', 'sequences']) {
  test(`CHECK-52/53/58/59/60/61/62 actual ${metric} snapshot`, async ({ request, page }) => {
    if (metric === 'queries') {
      seedQueryWorkload();
      await expect.poll(async () => JSON.stringify((await get(request,
        `/snapshots/latest?database=${target}&metric=queries`)).snapshot),
      { timeout: 20000 }).toContain('walkthrough_query_probe');
    }
    const snapshot = await get(request, `/snapshots/latest?database=${target}&metric=${metric}`);
    expect(snapshot.database).toBe(target);
    expect(snapshot.snapshot, 'Collector snapshot must exist').not.toBeNull();
    expect(typeof snapshot.snapshot).toBe('object');
    if (metric === 'tables') {
      await login(page);
      await page.getByTestId('database-picker').selectOption(target);
      await page.goto('/#/database');
      await expect(page.getByRole('heading', { name: `Database: ${target}` })).toBeVisible();
      await expect(page.locator('pre')).toContainText('{');
    }
  });
}

test('CHECK-63/64 snapshot history and invalid metric contracts', async ({ request }) => {
  const history = await get(request,
    `/snapshots/history?database=${target}&metric=cache_hit_ratio&hours=1`);
  expect(history.metric).toBe('cache_hit_ratio');
  rows(history.points);
  expect((await get(request, '/snapshots/latest?metric=nonexistent')).snapshot).toBeNull();
});

test('CHECK-68/69 database picker scopes Cases exactly', async ({ page, request }) => {
  await login(page);
  const picker = page.getByTestId('database-picker');
  await expect(picker).toBeVisible();
  for (const name of [target, secondTarget]) {
    await picker.selectOption(name);
    await open(page, 'cases', 'cases-page');
    const expected = rows((await get(request, `/cases?database=${name}`)).cases);
    expect(expected.every(c => c.database_name === name)).toBe(true);
    await expect(page.getByTestId('cases-page').locator('article')).toHaveCount(expected.length);
  }
});

for (const [route, key] of [
  ['forecasts', 'forecasts'], ['query-hints', 'hints'], ['alert-log', 'alerts'],
]) {
  test(`CHECK-80/81/82 ${route} database-scoped collection`, async ({ request }) => {
    const response = await get(request, `/${route}?database=${target}`);
    expect(response.database).toBe(target);
    rows(response[key]);
  });
}

test('CHECK-103/104 per-database execution policy persists; unsupported override fails',
  async ({ request }) => {
  const db = await managed(request);
  const route = `/config/databases/${db.id}`;
  expect((await get(request, route)).database_id).toBe(db.id);
  const before = object((await get(request, route)).config);
  const nextMode = object(before.execution_mode).value === 'manual' ? 'approval' : 'manual';
  await withConfig(request, route, { execution_mode: nextMode }, async () => {
    expect((await managed(request)).execution_mode).toBe(nextMode);
  });
  const current = await get(request, route);
  const rejected = await request.put(path(route), { data: {
    'analyzer.slow_query_threshold_ms': 750, expected_generation: current.desired_generation,
  } });
  expect(rejected.status()).toBe(400);
  expect(String(object(await rejected.json()).error)).toContain('not supported');
});

for (const route of [
  `/findings/999999?database=${target}`, '/databases/managed/999999',
  `/actions/999999?database=${target}`,
]) test(`CHECK-106/111/116 missing resource ${route}`, async ({ request }) => {
  const response = await body(await request.get(path(route)), 404);
  expect(String(response.error).length).toBeGreaterThan(0);
});

test('CHECK-108/109 duplicate user and malformed user ID fail clearly', async ({ request }) => {
  const duplicate = await request.post(path('/users'), {
    data: { email: adminEmail, password: `${unique()}!`, role: 'viewer' },
  });
  expect(duplicate.status()).toBe(409);
  expect((await request.delete(path('/users/notanumber'))).status()).toBe(400);
});

test('CHECK-93/94/95 missing pending actions reject approval and rejection',
  async ({ request }) => {
  for (const verb of ['approve', 'reject']) {
    const response = await request.post(path(`/actions/pending/999999/${verb}?database=${target}`),
      { data: { reason: 'isolated missing-id test' } });
    expect(response.status()).toBe(404);
  }
  expect((await request.post(path('/actions/pending/notanumber/approve'),
    { data: {} })).status()).toBe(404);
});

test('CHECK-113/118/119/121 invalid connection and execution requests fail',
  async ({ request }) => {
  const invalidHost = await request.post(path('/databases/managed/test-connection'), {
    data: { ...databaseInput(), host: 'nonexistent.invalid' },
  });
  expect(invalidHost.status()).toBe(400);
  expect(String(object(await invalidHost.json()).error).length).toBeGreaterThan(0);
  expect((await request.post(path('/actions/execute'), { data: {} })).status()).toBe(400);
  const invalidSQL = await request.post(path('/actions/execute'), {
    data: { finding_id: 999999, sql: 'SELECT 1', database: target },
  });
  expect(invalidSQL.status()).toBe(500);
  expect(String(object(await invalidSQL.json()).error)).toContain('SQL validation');
  expect((await request.post(path('/databases/managed/999999/test'),
    { data: {} })).status()).toBe(404);
});

test('CHECK-122/123/124 CSV imports exact row and rejects malformed input', async ({ request }) => {
  const input = databaseInput();
  let id: unknown;
  const upload = async (csv: string) => body(await request.post(path('/databases/managed/import'), {
    multipart: { file: { name: 'audit.csv', mimeType: 'text/csv', buffer: Buffer.from(csv) } },
  }));
  try {
    const csv = 'name,host,port,database_name,username,password,sslmode\n' +
      `${input.name},${input.host},${input.port},${input.database_name},postgres,postgres,disable`;
    const imported = await upload(csv);
    expect(imported.imported).toBe(1);
    expect(imported.errors).toEqual([]);
    id = (await managed(request, input.name)).id;
  } finally { if (id !== undefined) await remove(request, `/databases/managed/${id}`); }
  const invalid = await upload('bad,header,row\nfoo,bar,baz\n');
  expect(invalid.imported).toBe(0);
  expect(rows(invalid.errors)[0].error).toContain('invalid CSV header');
  expect((await request.post(path('/databases/managed/import'), { data: {} })).status()).toBe(400);
});
