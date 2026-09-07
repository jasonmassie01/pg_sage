import { APIRequestContext, APIResponse, expect, Page } from '@playwright/test';
import { createServer } from 'node:http';
import { randomUUID } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { fixtureTargets } from './fixture-targets';
import { login as loginForm } from './helpers';

export type Row = Record<string, unknown>;
export const adminEmail = process.env.PG_SAGE_ADMIN_EMAIL ?? 'admin@pg-sage.local';
export const adminPassword = process.env.PG_SAGE_ADMIN_PASS ?? '';
export const target = process.env.PG_SAGE_E2E_PROD_NAME ?? 'testdb';
export const secondTarget = process.env.PG_SAGE_E2E_STAGING_NAME ?? 'testdb2';
export const unique = () => `walkthrough-${randomUUID()}`;
export const path = (route: string) => `/api/v1${route}`;

export function object(value: unknown): Row {
  expect(value).not.toBeNull();
  expect(typeof value).toBe('object');
  expect(Array.isArray(value)).toBe(false);
  return value as Row;
}

export function rows(value: unknown): Row[] {
  expect(Array.isArray(value)).toBe(true);
  return (value as unknown[]).map(object);
}

export async function body(response: APIResponse, status = 200): Promise<Row> {
  expect(response.status(), await response.text()).toBe(status);
  return object(await response.json());
}

export const get = async (r: APIRequestContext, route: string) => body(await r.get(path(route)));
export const post = async (r: APIRequestContext, route: string, data: Row = {}, status = 200) =>
  body(await r.post(path(route), { data }), status);
export async function put(r: APIRequestContext, route: string, data: Row) {
  if (route.startsWith('/config/')) {
    const current = await get(r, route);
    data = { ...data, expected_generation: current.desired_generation };
  }
  return body(await r.put(path(route), { data }));
}

export async function apiLogin(r: APIRequestContext, email = adminEmail, password = adminPassword) {
  expect(password, 'PG_SAGE_ADMIN_PASS must be set; missing setup is a failure').not.toBe('');
  const user = await post(r, '/auth/login', { email, password });
  expect(user.email).toBe(email);
  return user;
}

export async function login(page: Page, email = adminEmail, password = adminPassword) {
  await loginForm(page, email, password);
  await expect(page.getByTestId('nav-value')).toBeVisible();
}

export async function open(page: Page, route: string, testId: string) {
  await page.goto(`/#/${route}`);
  await expect(page.getByTestId(testId)).toBeVisible();
}

export async function managed(r: APIRequestContext, name = target) {
  const databases = rows((await get(r, '/databases/managed')).databases);
  const database = databases.find(db => db.name === name);
  expect(database, `Required isolated fixture ${name} is missing`).toBeDefined();
  return database!;
}

export async function remove(r: APIRequestContext, route: string) {
  const response = await r.delete(path(route));
  expect(response.status(), await response.text()).toBe(200);
}

export async function withUser(
  r: APIRequestContext, role: string, run: (user: Row, password: string) => Promise<void>,
) {
  const password = `${randomUUID()}-Audit!`;
  const user = await post(r, '/users', { email: `${unique()}@test.local`, password, role }, 201);
  try { await run(user, password); } finally {
    await apiLogin(r);
    await remove(r, `/users/${user.id}`);
    expect(rows((await get(r, '/users')).users).some(u => u.id === user.id)).toBe(false);
  }
}

export async function withConfig(
  r: APIRequestContext, route: string, changes: Row, run: () => Promise<void>,
) {
  const before = object((await get(r, route)).config);
  try {
    await put(r, route, changes);
    const updated = object((await get(r, route)).config);
    for (const [key, value] of Object.entries(changes)) {
      const actual = object(updated[key]).value;
      if (key === 'llm.api_key') {
        expect(actual).not.toBe(value);
        expect(String(actual)).toMatch(/^\*+/);
      } else expect(actual).toBe(value);
    }
    await run();
  } finally {
    for (const key of Object.keys(changes)) {
      const entry = object(before[key]);
      if (entry.source === 'db_override' || entry.source === 'override'
          && route === '/config/global') {
        await put(r, route, { [key]: entry.value });
      } else {
        const current = await get(r, route);
        await remove(r, `${route}/${key}?expected_generation=${current.desired_generation}`);
      }
    }
    const restored = object((await get(r, route)).config);
    for (const key of Object.keys(changes)) expect(restored[key]).toEqual(before[key]);
  }
}

export async function localSink() {
  const received: Row[] = [];
  const server = createServer((req, res) => {
    let payload = '';
    req.setEncoding('utf8');
    req.on('data', chunk => { payload += chunk; });
    req.on('end', () => {
      received.push({ method: req.method, url: req.url, payload });
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ data: [{ id: 'local-audit-model' }] }));
    });
  });
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Local sink did not bind');
  return {
    url: `http://127.0.0.1:${address.port}`, received,
    close: () => new Promise<void>((resolve, reject) => {
      server.close(error => error ? reject(error) : resolve());
      server.closeAllConnections();
    }),
  };
}

async function createNotificationRule(page: Page, r: APIRequestContext, channelId: unknown) {
    await page.getByTestId('notifications-tab-rules').click();
    await page.getByTestId('add-rule-channel').selectOption(String(channelId));
    await page.getByTestId('add-rule-event').selectOption('finding_critical');
    await page.getByTestId('add-rule-severity').selectOption('critical');
    await page.getByTestId('add-rule-submit').click();
    await expect(page.getByTestId('rules-table')).toContainText('finding_critical');
    const ruleId = rows((await get(r, '/notifications/rules')).rules)
      .find(rule => rule.channel_id === channelId)?.id;
    expect(ruleId).toBeDefined();
    await put(r, `/notifications/rules/${ruleId}`, { enabled: false });
    const rule = rows((await get(r, '/notifications/rules')).rules).find(v => v.id === ruleId);
    expect(rule?.enabled).toBe(false);
    return ruleId;
}

export async function notificationLifecycle(page: Page, r: APIRequestContext) {
  const sink = await localSink();
  const name = unique();
  let channelId: unknown;
  let ruleId: unknown;
  try {
    await open(page, 'notifications', 'notifications-tab-channels');
    await page.getByTestId('add-channel-name').fill(name);
    await page.getByTestId('add-channel-type').selectOption('slack');
    await page.getByTestId('add-channel-webhook-url').fill(`${sink.url}/slack`);
    await page.getByTestId('add-channel-submit').click();
    await expect(page.getByTestId('channels-table')).toContainText(name);
    channelId = rows((await get(r, '/notifications/channels')).channels)
      .find(channel => channel.name === name)?.id;
    expect(channelId).toBeDefined();
    ruleId = await createNotificationRule(page, r, channelId);
    await put(r, `/notifications/channels/${channelId}`, { name: `${name}-edited`, enabled: true });
    const channel = rows((await get(r, '/notifications/channels')).channels)
      .find(v => v.id === channelId);
    expect(channel?.name).toBe(`${name}-edited`);
    await post(r, `/notifications/channels/${channelId}/test`);
    await expect.poll(() => sink.received.length).toBeGreaterThan(0);
    const receipt = sink.received.find(v => v.url === '/slack');
    expect(receipt?.method).toBe('POST');
    expect(rows(object(JSON.parse(String(receipt?.payload))).blocks).length).toBeGreaterThan(0);
    await page.getByTestId('notifications-tab-log').click();
    await expect(page.getByTestId('notifications-tab-log')).toBeVisible();
  } finally {
    if (ruleId !== undefined) await remove(r, `/notifications/rules/${ruleId}`);
    if (channelId !== undefined) await remove(r, `/notifications/channels/${channelId}`);
    await sink.close();
    expect(rows((await get(r, '/notifications/channels')).channels)
      .some(v => v.id === channelId)).toBe(false);
  }
}

export const databaseInput = () => ({
  name: unique(), host: '127.0.0.1', port: 5455,
  database_name: 'audit_browser_target1', username: 'postgres', password: 'postgres',
  sslmode: 'disable', trust_level: 'observation', execution_mode: 'approval',
});

export async function fillDatabase(page: Page, input: ReturnType<typeof databaseInput>) {
  for (const [testId, value] of [
    ['name', input.name], ['host', input.host], ['port', input.port],
    ['database', input.database_name], ['username', input.username], ['password', input.password],
  ]) await page.getByTestId(`db-${testId}`).fill(String(value));
  await page.getByTestId('db-sslmode').selectOption(input.sslmode);
}

export async function databaseLifecycle(page: Page, r: APIRequestContext) {
  const input = databaseInput();
  let id: unknown;
  try {
    await open(page, 'manage-databases', 'add-database-button');
    await page.getByTestId('add-database-button').click();
    await fillDatabase(page, input);
    await page.getByTestId('db-test-connection').click();
    await expect(page.getByTestId('db-form')).toContainText('private/internal');
    await page.getByTestId('db-save-button').click();
    const row = page.getByTestId('db-row').filter({ hasText: input.name });
    await expect(row).toBeVisible();
    const created = await managed(r, input.name);
    id = created.id;
    const connection = await body(await r.post(path(`/databases/managed/${id}/test`), {
      headers: { 'Content-Type': 'application/json' },
    }));
    expect(connection.status).toBe('ok');
    expect(String(connection.pg_version)).toContain('PostgreSQL');
    expect(Array.isArray(connection.extensions)).toBe(true);
    await row.getByTestId('db-edit-button').click();
    await expect(page.getByTestId('db-name')).toHaveValue(input.name);
    await expect(page.getByTestId('db-password')).toHaveValue('');
    await page.getByTestId('db-cancel-button').click();
    await verifyDatabaseEdit(r, id, input);
    await row.getByTestId('db-delete-button').click();
    await page.getByTestId('delete-confirm-yes').click();
    await expect(row).toHaveCount(0);
    expect(rows((await get(r, '/databases/managed')).databases).some(v => v.id === id)).toBe(false);
    id = undefined;
    const recreated = await post(r, '/databases/managed', input, 201);
    id = recreated.id;
    expect((await managed(r, input.name)).name).toBe(input.name);
  } finally {
    if (id !== undefined) await remove(r, `/databases/managed/${id}`);
  }
}

export function seedQueryWorkload() {
  const fixture = fixtureTargets.find(db => db.name === target);
  if (!fixture) throw new Error('Query workload requires an explicit disposable fixture');
  const result = execFileSync('docker', ['exec', fixture.container, 'psql', '-U', 'postgres',
    '-d', fixture.db, '-At', '-c',
    'SELECT count(*) FROM generate_series(1,300) AS walkthrough_query_probe(id)'],
  { encoding: 'utf8' });
  expect(result.trim()).toBe('300');
}

async function verifyDatabaseEdit(
  r: APIRequestContext, id: unknown, input: ReturnType<typeof databaseInput>,
) {
  const rejected = await r.put(path(`/databases/managed/${id}`), {
    data: { ...input, trust_level: 'advisory' },
  });
  expect(rejected.status()).toBe(400);
  expect(String(object(await rejected.json()).error)).toContain('database Settings');
  await put(r, `/databases/managed/${id}`, { ...input, max_connections: 3 });
  expect((await get(r, `/databases/managed/${id}`)).max_connections).toBe(3);
}
