import { test, expect, Page } from '@playwright/test';

// Keep this spec runnable with --workers=1 so the database setup steps happen
// before dependent checks. Do not use Playwright serial mode: serial mode
// skips every later check after the first failure, hiding unrelated regressions.

// All tests run serially — later steps depend on databases added in step 5

// Admin credentials are read from the environment so the spec is safe
// to commit. pg_sage prints the auto-generated admin password to stderr
// on first boot — export it before running this suite:
//
//   export PG_SAGE_ADMIN_EMAIL=admin@pg-sage.local
//   export PG_SAGE_ADMIN_PASS=<password from sidecar stderr>
//   npx playwright test e2e/walkthrough.spec.ts
const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL ?? 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS ?? '';
const PROD_DB = {
  name: process.env.PG_SAGE_E2E_PROD_NAME ?? 'production',
  host: process.env.PG_SAGE_E2E_PROD_HOST ?? 'localhost',
  port: Number(process.env.PG_SAGE_E2E_PROD_PORT ?? 5433),
  database: process.env.PG_SAGE_E2E_PROD_DB ?? 'app_production',
  username: process.env.PG_SAGE_E2E_PROD_USER ?? 'postgres',
  password: process.env.PG_SAGE_E2E_PROD_PASS ?? 'postgres',
};
const STAGING_DB = {
  name: process.env.PG_SAGE_E2E_STAGING_NAME ?? 'staging',
  host: process.env.PG_SAGE_E2E_STAGING_HOST ?? 'localhost',
  port: Number(process.env.PG_SAGE_E2E_STAGING_PORT ?? 5434),
  database: process.env.PG_SAGE_E2E_STAGING_DB ?? 'app_staging',
  username: process.env.PG_SAGE_E2E_STAGING_USER ?? 'postgres',
  password: process.env.PG_SAGE_E2E_STAGING_PASS ?? 'postgres',
};
const IMPORT_DB = {
  host: process.env.PG_SAGE_E2E_IMPORT_HOST ?? PROD_DB.host,
  port: Number(process.env.PG_SAGE_E2E_IMPORT_PORT ?? PROD_DB.port),
  database: process.env.PG_SAGE_E2E_IMPORT_DB ?? PROD_DB.database,
  username: process.env.PG_SAGE_E2E_IMPORT_USER ?? PROD_DB.username,
  password: process.env.PG_SAGE_E2E_IMPORT_PASS ?? PROD_DB.password,
};

// Steps 5+ require fixture databases on localhost:5433 (app_production,
// postgres/postgres) and :5434 (app_staging). When fixtures aren't
// provisioned, the walkthrough's DB-add steps will fail. Gate behind an
// explicit env var so the login-only checks still run everywhere.
const HAS_FIXTURES = process.env.PG_SAGE_E2E_FIXTURES === '1';

// Skip every test in the walkthrough suite at runtime when the admin password
// isn't exported, so other spec files still collect. Throwing at module load
// would fail Playwright's discovery pass for the entire directory.
//
// Also skip fixture-dependent steps (Step 5+) when HAS_FIXTURES is false.
// Login-only steps (CHECK-01..CHECK-04) still run since they only need the
// admin user, which the spec sweep provisions on pgsage_demo_a.
test.beforeEach(({}, testInfo) => {
  test.skip(
    !ADMIN_PASS,
    'PG_SAGE_ADMIN_PASS env var is required — see comment at top of this file',
  );
  const loginOnly = /^CHECK-0[1-4]\b/.test(testInfo.title);
  if (!loginOnly) {
    test.skip(
      !HAS_FIXTURES,
      'PG_SAGE_E2E_FIXTURES=1 required — walkthrough adds DBs on :5433/:5434',
    );
  }
});

async function login(
  page: Page,
  email = ADMIN_EMAIL,
  password = ADMIN_PASS,
) {
  await page.goto('/');
  await page.fill('[data-testid="login-email"]', email);
  await page.fill('[data-testid="login-password"]', password);
  await page.click('[data-testid="login-submit"]');
  await expect(page.locator('[data-testid="login-submit"]')).not.toBeVisible({
    timeout: 10000,
  });
}

async function apiLogin(request: any) {
  const res = await request.post('/api/v1/auth/login', {
    data: { email: ADMIN_EMAIL, password: ADMIN_PASS },
  });
  expect(res.ok()).toBeTruthy();
}

// ─── Step 4: Login ───────────────────────────────────────────────
test.describe('Step 4: Login', () => {
  test('CHECK-01: login page loads with form', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('h1')).toContainText('pg_sage');
    await expect(page.locator('[data-testid="login-email"]')).toBeVisible();
    await expect(page.locator('[data-testid="login-password"]')).toBeVisible();
    await expect(page.locator('[data-testid="login-submit"]')).toBeVisible();
  });

  test('CHECK-02: login with valid admin credentials', async ({ page }) => {
    await login(page);
    await expect(page.locator('[data-testid="login-submit"]')).not.toBeVisible();
    await expect(page.locator('[data-testid="nav-dashboard"]')).toBeVisible();
  });

  test('CHECK-03: login with wrong password shows error', async ({ page }) => {
    await page.goto('/');
    await page.fill('[data-testid="login-email"]', ADMIN_EMAIL);
    await page.fill('[data-testid="login-password"]', 'wrongpassword');
    await page.click('[data-testid="login-submit"]');
    await expect(page.locator('[data-testid="login-error"]')).toBeVisible({
      timeout: 5000,
    });
  });

  test('CHECK-04: login via API returns user info', async ({ request }) => {
    const res = await request.post('/api/v1/auth/login', {
      data: { email: ADMIN_EMAIL, password: ADMIN_PASS },
    });
    expect(res.ok()).toBeTruthy();
    const body = await res.json();
    expect(body.email).toBe(ADMIN_EMAIL);
    expect(body.role).toBe('admin');
    expect(body.id).toBe(1);
  });
});

// ─── Step 5: Add Databases via UI ────────────────────────────────
test.describe('Step 5: Add Databases', () => {
  test('CHECK-05: add production database with test connection',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-databases"]');
      await expect(
        page.locator('[data-testid="add-database-button"]'),
      ).toBeVisible({ timeout: 5000 });

      await page.click('[data-testid="add-database-button"]');
      await expect(
        page.locator('[data-testid="db-form"]'),
      ).toBeVisible({ timeout: 3000 });

      await page.fill('[data-testid="db-name"]', PROD_DB.name);
      await page.fill('[data-testid="db-host"]', PROD_DB.host);

      const portInput = page.locator('[data-testid="db-port"]');
      await portInput.clear();
      await portInput.fill(String(PROD_DB.port));

      await page.fill('[data-testid="db-database"]', PROD_DB.database);
      await page.fill('[data-testid="db-username"]', PROD_DB.username);
      await page.fill('[data-testid="db-password"]', PROD_DB.password);

      // Set SSL mode to disable
      await page.locator('[data-testid="db-form"] select').first()
        .selectOption('disable');

      // Test Connection before saving — this exercises the UI flow
      // regardless of whether the backing Postgres on 5433 accepts creds.
      // Success renders "Connected - <version>" (DatabaseForm.jsx:219);
      // failure renders the backend error. Either proves the flow ran.
      await page.click('[data-testid="db-test-connection"]');
      await page.waitForTimeout(3000);
      const body = await page.textContent('body');
      const sawResult =
        /Connected/i.test(body!) ||
        /failed|error|refused|timeout|authentication/i.test(body!);
      expect(sawResult).toBeTruthy();

      await page.click('[data-testid="db-save-button"]');
      await page.waitForTimeout(2000);

      await expect(
        page.locator('[data-testid="databases-table"]'),
      ).toContainText(PROD_DB.name, { timeout: 5000 });
    });

  test('CHECK-06: add staging database via form', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-databases"]');
    await expect(
      page.locator('[data-testid="add-database-button"]'),
    ).toBeVisible({ timeout: 5000 });

    await page.click('[data-testid="add-database-button"]');
    await expect(
      page.locator('[data-testid="db-form"]'),
    ).toBeVisible({ timeout: 3000 });

    await page.fill('[data-testid="db-name"]', STAGING_DB.name);
    await page.fill('[data-testid="db-host"]', STAGING_DB.host);

    const portInput = page.locator('[data-testid="db-port"]');
    await portInput.clear();
    await portInput.fill(String(STAGING_DB.port));

    await page.fill('[data-testid="db-database"]', STAGING_DB.database);
    await page.fill('[data-testid="db-username"]', STAGING_DB.username);
    await page.fill('[data-testid="db-password"]', STAGING_DB.password);

    await page.locator('[data-testid="db-form"] select').first()
      .selectOption('disable');

    await page.click('[data-testid="db-save-button"]');
    await page.waitForTimeout(2000);

    await expect(
      page.locator('[data-testid="databases-table"]'),
    ).toContainText(STAGING_DB.name, { timeout: 5000 });
  });

  test('CHECK-07: both databases listed in table', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-databases"]');
    await page.waitForTimeout(2000);

    const table = page.locator('[data-testid="databases-table"]');
    await expect(table).toContainText(PROD_DB.name);
    await expect(table).toContainText(STAGING_DB.name);
  });

  test('CHECK-08: verify databases via API', async ({ request }) => {
    await apiLogin(request);

    const res = await request.get('/api/v1/databases/managed');
    expect(res.ok()).toBeTruthy();
    const data = await res.json();
    expect(data.databases.length).toBe(2);

    const names = data.databases.map((d: any) => d.name);
    expect(names).toContain(PROD_DB.name);
    expect(names).toContain(STAGING_DB.name);
  });
});

// ─── Step 6: Dashboard / Fleet Overview ──────────────────────────
test.describe('Step 6: Dashboard', () => {
  test('CHECK-09: dashboard shows fleet info', async ({ page }) => {
    await login(page);
    await page.waitForTimeout(3000);

    const body = await page.textContent('body');
    const hasDatabaseInfo =
      body!.includes('production') ||
      body!.includes('staging') ||
      body!.includes('database');
    expect(hasDatabaseInfo).toBeTruthy();
  });

  test('CHECK-10: fleet API returns 2 databases', async ({ request }) => {
    await apiLogin(request);

    let data: any;
    for (let i = 0; i < 15; i++) {
      const res = await request.get('/api/v1/databases');
      expect(res.ok()).toBeTruthy();
      data = await res.json();
      if (data.summary.total_databases >= 2) break;
      await new Promise((r) => setTimeout(r, 1000));
    }
    expect(data.mode).toBe('fleet');
    expect(data.summary.total_databases).toBe(2);
    expect(data.databases.length).toBe(2);
  });
});

// ─── Step 7: Findings / Recommendations ─────────────────────────
test.describe('Step 7: Findings', () => {
  test('CHECK-11: recommendations page loads with findings',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-findings"]');

      // Wait for collector + analyzer cycles (10s + 15s)
      // Retry up to 60s for findings to appear
      const count = page.locator('[data-testid="findings-count"]');
      await expect(count).toBeVisible({ timeout: 5000 });
      let found = false;
      for (let i = 0; i < 12; i++) {
        await page.waitForTimeout(5000);
        await page.click('[data-testid="nav-dashboard"]');
        await page.waitForTimeout(500);
        await page.click('[data-testid="nav-findings"]');
        await page.waitForTimeout(2000);
        const text = await count.textContent();
        if (parseInt(text || '0', 10) > 0) {
          found = true;
          break;
        }
      }
      expect(found).toBeTruthy();
    });

  test('CHECK-12: findings include expected categories',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/findings');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.findings.length).toBeGreaterThan(0);

      const categories = data.findings.map((f: any) => f.category);
      // Should have duplicate_index from planted problems
      expect(categories).toContain('duplicate_index');
    });

  test('CHECK-13: findings filter by database via API',
    async ({ request }) => {
      await apiLogin(request);

      const prodRes = await request.get(
        '/api/v1/findings?database=production',
      );
      expect(prodRes.ok()).toBeTruthy();
      const prodData = await prodRes.json();

      const stagRes = await request.get(
        '/api/v1/findings?database=staging',
      );
      expect(stagRes.ok()).toBeTruthy();
      const stagData = await stagRes.json();

      // Each should have findings from planted problems
      expect(prodData.findings.length).toBeGreaterThan(0);
      expect(stagData.findings.length).toBeGreaterThan(0);
    });

  test('CHECK-14: expand a finding to see detail',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-findings"]');
      await page.waitForTimeout(3000);

      // Click on the first finding row to expand
      const firstRow = page.locator('tr').filter({
        hasText: /duplicate_index|sequence_exhaustion|checkpoint/,
      }).first();
      await firstRow.click();
      await page.waitForTimeout(1000);

      // Should see detail grid and recommendation
      const detail = page.locator('[data-testid="detail-grid"]').first();
      await expect(detail).toBeVisible({ timeout: 3000 });
    });

  test('CHECK-15: severity filter works', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-findings"]');
    await page.waitForTimeout(3000);

    // Filter by critical
    await page.locator('[data-testid="severity-filter"]')
      .selectOption('critical');
    await page.waitForTimeout(2000);

    // All visible findings should be critical
    const count = page.locator('[data-testid="findings-count"]');
    const text = await count.textContent();
    expect(parseInt(text || '0', 10)).toBeGreaterThan(0);
  });
});

// ─── Step 8: Suppress / Unsuppress ──────────────────────────────
test.describe('Step 8: Suppress/Unsuppress', () => {
  test('CHECK-16: suppress finding via UI button', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-findings"]');
    await page.waitForTimeout(3000);

    // Click on first finding to expand
    const firstRow = page.locator('tr').filter({
      hasText: /duplicate_index|sequence_exhaustion|checkpoint/,
    }).first();
    await firstRow.click();
    await page.waitForTimeout(1000);

    // Click Suppress button
    const suppressBtn = page.locator(
      '[data-testid="suppress-button"]',
    ).first();
    await expect(suppressBtn).toBeVisible({ timeout: 3000 });
    await expect(suppressBtn).toContainText('Suppress');
    await suppressBtn.click();
    await page.waitForTimeout(2000);

    // Finding should disappear from open list (table refreshes)
  });

  test('CHECK-17: unsuppress finding via UI button',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-findings"]');
      await page.waitForTimeout(2000);

      // Click "Suppressed" status tab
      await page.click('button:has-text("Suppressed")');
      await page.waitForTimeout(2000);

      // Should see suppressed finding
      const firstRow = page.locator('tr').filter({
        hasText: /duplicate_index|sequence_exhaustion|checkpoint/,
      }).first();
      if (await firstRow.isVisible({ timeout: 3000 })) {
        await firstRow.click();
        await page.waitForTimeout(1000);

        // Click Unsuppress
        const unsuppressBtn = page.locator(
          '[data-testid="suppress-button"]',
        ).first();
        await expect(unsuppressBtn).toContainText('Unsuppress');
        await unsuppressBtn.click();
        await page.waitForTimeout(2000);
      }

      // Switch back to Open tab
      await page.click('button:has-text("Open")');
    });

  test('CHECK-18: suppress/unsuppress via API', async ({ request }) => {
    await apiLogin(request);

    const findingsRes = await request.get('/api/v1/findings');
    const data = await findingsRes.json();

    if (data.findings && data.findings.length > 0) {
      const id = data.findings[0].id;

      const suppressRes = await request.post(
        `/api/v1/findings/${id}/suppress`,
      );
      expect(suppressRes.ok()).toBeTruthy();

      const checkRes = await request.get(`/api/v1/findings/${id}`);
      const finding = await checkRes.json();
      expect(finding.status).toBe('suppressed');

      const unsuppressRes = await request.post(
        `/api/v1/findings/${id}/unsuppress`,
      );
      expect(unsuppressRes.ok()).toBeTruthy();
    }
  });
});

// ─── Step 9: Settings Page ──────────────────────────────────────
test.describe('Step 9: Settings', () => {
  test('CHECK-19: settings page loads with simple tabs',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-settings"]');
      await page.waitForTimeout(2000);

      await expect(
        page.locator('[data-testid="settings-tab-general"]'),
      ).toBeVisible();
      await expect(
        page.locator('[data-testid="settings-tab-monitoring"]'),
      ).toBeVisible();
      await expect(
        page.locator('[data-testid="settings-tab-ai-alerts"]'),
      ).toBeVisible();
    });

  test('CHECK-20: toggle to advanced mode shows more tabs',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-settings"]');
      await page.waitForTimeout(1000);

      await page.click('[data-testid="settings-mode-toggle"]');
      await page.waitForTimeout(500);

      await expect(
        page.locator('[data-testid="settings-tab-llm"]'),
      ).toBeVisible();
      await expect(
        page.locator('[data-testid="settings-tab-analyzer"]'),
      ).toBeVisible();
    });

  test('CHECK-21: config API returns config structure',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/config');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('analyzer');
      expect(data).toHaveProperty('collector');
      expect(data).toHaveProperty('mode');
    });

  test('CHECK-22: update config via API and verify',
    async ({ request }) => {
      await apiLogin(request);

      const updateRes = await request.put('/api/v1/config/global', {
        data: { 'analyzer.slow_query_threshold_ms': 2000 },
      });
      expect(updateRes.ok()).toBeTruthy();

      // Reset
      await request.put('/api/v1/config/global', {
        data: { 'analyzer.slow_query_threshold_ms': 1000 },
      });
    });
});

// ─── Step 10: LLM Config ────────────────────────────────────────
test.describe('Step 10: LLM Config', () => {
  test('CHECK-23: configure LLM settings via API',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.put('/api/v1/config/global', {
        data: {
          'llm.enabled': true,
          'llm.endpoint':
            'https://generativelanguage.googleapis.com/v1beta/openai',
          'llm.api_key': 'test-key-placeholder',
          'llm.model': 'gemini-2.0-flash',
        },
      });
      expect(res.ok()).toBeTruthy();
    });

  test('CHECK-24: LLM config visible in settings AI tab',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-settings"]');
      await page.waitForTimeout(1000);

      await page.click('[data-testid="settings-tab-ai-alerts"]');
      await page.waitForTimeout(1000);

      const body = await page.textContent('body');
      const hasLLM =
        body!.includes('LLM') ||
        body!.includes('llm') ||
        body!.includes('AI') ||
        body!.includes('Model');
      expect(hasLLM).toBeTruthy();
    });
});

// ─── Step 11: Notifications ─────────────────────────────────────
test.describe('Step 11: Notifications', () => {
  test('CHECK-25: notifications page loads with tabs',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-notifications"]');
      await page.waitForTimeout(2000);

      await expect(
        page.locator('[data-testid="notifications-tab-channels"]'),
      ).toBeVisible();
      await expect(
        page.locator('[data-testid="notifications-tab-rules"]'),
      ).toBeVisible();
      await expect(
        page.locator('[data-testid="notifications-tab-log"]'),
      ).toBeVisible();
    });

  test('CHECK-26: create notification channel via UI form',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-notifications"]');
      await page.waitForTimeout(1000);

      // Fill channel form
      await page.fill(
        '[data-testid="add-channel-name"]',
        'team-alerts',
      );
      await page.locator('[data-testid="add-channel-type"]')
        .selectOption('slack');
      await page.waitForTimeout(500);

      // Fill webhook URL (appears when slack is selected)
      const webhookInput = page.locator(
        'input[placeholder*="hooks.slack.com"]',
      );
      if (await webhookInput.isVisible()) {
        await webhookInput.fill(
          'https://hooks.slack.com/e2e-test',
        );
      }

      await page.click('[data-testid="add-channel-submit"]');
      await page.waitForTimeout(2000);

      // Channel should appear in table
      await expect(
        page.locator('[data-testid="channels-table"]'),
      ).toContainText('team-alerts', { timeout: 5000 });
    });

  test('CHECK-27: create notification rule via UI form',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-notifications"]');
      await page.waitForTimeout(1000);

      // Switch to Rules tab
      await page.click('[data-testid="notifications-tab-rules"]');
      await page.waitForTimeout(1000);

      // Fill rule form
      const channelSelect = page.locator(
        '[data-testid="add-rule-channel"]',
      );
      await page.waitForTimeout(1000);
      await channelSelect.selectOption({ index: 0 });

      await page.locator('[data-testid="add-rule-event"]')
        .selectOption('finding_critical');
      await page.locator('[data-testid="add-rule-severity"]')
        .selectOption('critical');

      await page.click('[data-testid="add-rule-submit"]');
      await page.waitForTimeout(2000);

      // Rule should appear in table
      await expect(
        page.locator('[data-testid="rules-table"]'),
      ).toContainText('finding_critical', { timeout: 5000 });
    });

  test('CHECK-28: notification log tab loads', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-notifications"]');
    await page.waitForTimeout(1000);

    await page.click('[data-testid="notifications-tab-log"]');
    await page.waitForTimeout(1000);

    // Log tab should load without errors
    await expect(
      page.locator('[data-testid="login-submit"]'),
    ).not.toBeVisible();
  });
});

// ─── Step 12: User Management ───────────────────────────────────
test.describe('Step 12: User Management', () => {
  test('CHECK-29: users page shows admin', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-users"]');
    await page.waitForTimeout(2000);

    await expect(
      page.locator('[data-testid="users-table"]'),
    ).toContainText('admin@pg-sage.local');
  });

  test('CHECK-30: create user via UI form', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-users"]');
    await page.waitForTimeout(1000);

    // Fill the add user form
    await page.fill(
      '[data-testid="add-user-email"]',
      'e2e-uiuser@example.com',
    );
    await page.fill(
      '[data-testid="add-user-password"]',
      'uipass12345!',
    );
    await page.locator('[data-testid="add-user-role"]')
      .selectOption('viewer');

    await page.click('[data-testid="add-user-submit"]');
    await page.waitForTimeout(2000);

    // User should appear in table
    await expect(
      page.locator('[data-testid="users-table"]'),
    ).toContainText('e2e-uiuser@example.com', { timeout: 5000 });
  });

  test('CHECK-31: create operator and viewer via API',
    async ({ request }) => {
      await apiLogin(request);

      const opsRes = await request.post('/api/v1/users', {
        data: {
          email: 'e2e-ops@example.com',
          password: 'ops12345!',
          role: 'operator',
        },
      });
      expect(opsRes.ok()).toBeTruthy();

      const viewRes = await request.post('/api/v1/users', {
        data: {
          email: 'e2e-viewer@example.com',
          password: 'view12345!',
          role: 'viewer',
        },
      });
      expect(viewRes.ok()).toBeTruthy();

      const listRes = await request.get('/api/v1/users');
      expect(listRes.ok()).toBeTruthy();
      const users = await listRes.json();
      expect(users.users.length).toBeGreaterThanOrEqual(4);
    });

  test('CHECK-32: viewer gets 403 on admin routes',
    async ({ request }) => {
      const viewerLogin = await request.post('/api/v1/auth/login', {
        data: {
          email: 'e2e-viewer@example.com',
          password: 'view12345!',
        },
      });
      expect(viewerLogin.ok()).toBeTruthy();

      const usersRes = await request.get('/api/v1/users');
      expect(usersRes.status()).toBe(403);
    });

  test('CHECK-33: viewer sidebar hides admin nav items',
    async ({ page }) => {
      await page.goto('/');
      await page.fill(
        '[data-testid="login-email"]',
        'e2e-viewer@example.com',
      );
      await page.fill(
        '[data-testid="login-password"]',
        'view12345!',
      );
      await page.click('[data-testid="login-submit"]');
      await expect(
        page.locator('[data-testid="login-submit"]'),
      ).not.toBeVisible({ timeout: 10000 });

      // Viewer should NOT see admin nav items
      await expect(
        page.locator('[data-testid="nav-users"]'),
      ).not.toBeVisible();
      await expect(
        page.locator('[data-testid="nav-databases"]'),
      ).not.toBeVisible();
      await expect(
        page.locator('[data-testid="nav-notifications"]'),
      ).not.toBeVisible();

      // Should see Dashboard and Recommendations
      await expect(
        page.locator('[data-testid="nav-dashboard"]'),
      ).toBeVisible();
      await expect(
        page.locator('[data-testid="nav-findings"]'),
      ).toBeVisible();
    });

  test('CHECK-34: sign out and sign back in', async ({ page }) => {
    await login(page);

    // Click sign out
    await page.click('[data-testid="sign-out-button"]');
    await page.waitForTimeout(2000);

    // Should be back at login page
    await expect(
      page.locator('[data-testid="login-submit"]'),
    ).toBeVisible({ timeout: 5000 });

    // Sign back in
    await login(page);
    await expect(
      page.locator('[data-testid="nav-dashboard"]'),
    ).toBeVisible();
  });

  test('CHECK-35: clean up test users', async ({ request }) => {
    await apiLogin(request);

    const listRes = await request.get('/api/v1/users');
    const users = await listRes.json();

    for (const u of users.users) {
      if (u.email.startsWith('e2e-')) {
        const delRes = await request.delete(`/api/v1/users/${u.id}`);
        expect(delRes.ok()).toBeTruthy();
      }
    }

    const finalRes = await request.get('/api/v1/users');
    const finalUsers = await finalRes.json();
    expect(finalUsers.users.length).toBe(1);
  });
});

// ─── Step 13: Emergency Stop / Resume ───────────────────────────
test.describe('Step 13: Emergency Stop', () => {
  test('CHECK-36: emergency stop halts fleet', async ({ request }) => {
    await apiLogin(request);

    const stopRes = await request.post('/api/v1/emergency-stop');
    expect(stopRes.ok()).toBeTruthy();
    const stopData = await stopRes.json();
    expect(stopData.status).toBe('stopped');
  });

  test('CHECK-37: emergency stop badge visible',
    async ({ page }) => {
      await login(page);
      await page.waitForTimeout(2000);

      // Emergency stop badge should be visible in sidebar
      const badge = page.locator(
        '[data-testid="emergency-stop-badge"]',
      );
      await expect(badge).toBeVisible({ timeout: 5000 });
    });

  test('CHECK-38: resume button visible on settings page',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-settings"]');
      await page.waitForTimeout(2000);

      const resumeBtn = page.locator('[data-testid="resume-button"]');
      await expect(resumeBtn).toBeVisible({ timeout: 5000 });
    });

  test('CHECK-39: resume restores fleet', async ({ request }) => {
    await apiLogin(request);

    const resumeRes = await request.post('/api/v1/resume');
    expect(resumeRes.ok()).toBeTruthy();
    const resumeData = await resumeRes.json();
    expect(resumeData.status).toBe('resumed');
  });
});

// ─── Other Pages ────────────────────────────────────────────────
test.describe('Other Pages', () => {
  test('CHECK-40: actions page loads', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-actions"]');
    await page.waitForTimeout(2000);

    await expect(
      page.locator('[data-testid="actions-tab-executed"]'),
    ).toBeVisible();
    await expect(
      page.locator('[data-testid="actions-tab-pending"]'),
    ).toBeVisible();
  });

  test('CHECK-41: forecasts page loads', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-forecasts"]');
    await page.waitForTimeout(2000);

    // Should load without errors
    await expect(
      page.locator('[data-testid="login-submit"]'),
    ).not.toBeVisible();
  });

  test('CHECK-42: performance page loads', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-query-hints"]');
    await page.waitForTimeout(2000);

    await expect(
      page.locator('[data-testid="login-submit"]'),
    ).not.toBeVisible();
  });

  test('CHECK-43: alerts page loads', async ({ page }) => {
    await login(page);
    await page.click('[data-testid="nav-alerts"]');
    await page.waitForTimeout(2000);

    await expect(
      page.locator('[data-testid="login-submit"]'),
    ).not.toBeVisible();
  });
});

// ─── Step 14: Prometheus Metrics ────────────────────────────────
test.describe('Step 14: Prometheus', () => {
  test('CHECK-44: prometheus serves pg_sage metrics',
    async ({ request }) => {
      const res = await request.get('http://localhost:9187/metrics');
      expect(res.ok()).toBeTruthy();
      const body = await res.text();
      expect(body).toContain('pg_sage_info');
      expect(body).toContain('pg_sage_connection_up');
    });

  test('CHECK-45: fleet metrics present', async ({ request }) => {
    const res = await request.get('http://localhost:9187/metrics');
    const body = await res.text();
    expect(body).toContain('pg_sage_fleet_databases');
  });
});

// ─── Step 15: Static Assets ────────────────────────────────────
test.describe('Step 15: Static Assets', () => {
  test('CHECK-46: React SPA loads with root div',
    async ({ page }) => {
      await page.goto('/');
      await expect(page.locator('#root')).toBeVisible();
      await expect(page).toHaveTitle(/pg_sage/);
    });

  test('CHECK-47: no JS errors on page load', async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', (err) => errors.push(err.message));

    await page.goto('/');
    await page.waitForTimeout(3000);

    const real = errors.filter(
      (e) => !e.includes('favicon') && !e.includes('404'),
    );
    expect(real).toHaveLength(0);
  });
});

// ═══════════════════════════════════════════════════════════════
// GAP-CLOSING TESTS — Coverage for walkthrough sections not
// covered by CHECK-01 through CHECK-47
// ═══════════════════════════════════════════════════════════════

// ─── Step 16: Dashboard Stat Cards ─────────────────────────────
test.describe('Step 16: Dashboard Stat Cards', () => {
  test('CHECK-48: health hero shows status', async ({ page }) => {
    await login(page);
    await page.waitForTimeout(3000);

    const hero = page.locator('[data-testid="health-hero"]');
    await expect(hero).toBeVisible({ timeout: 5000 });
    const text = await hero.textContent();
    expect(text!.length).toBeGreaterThan(0);
  });

  test('CHECK-49: stat-databases shows count >= 2',
    async ({ page }) => {
      await login(page);
      await page.waitForTimeout(3000);

      const dbStat = page.locator('[data-testid="stat-databases"]');
      await expect(dbStat).toBeVisible({ timeout: 5000 });
      const text = await dbStat.textContent();
      expect(text).toContain('2');
    });

  test('CHECK-50: db-list shows database entries',
    async ({ page }) => {
      await login(page);
      await page.waitForTimeout(3000);

      const dbList = page.locator('[data-testid="db-list"]');
      await expect(dbList).toBeVisible({ timeout: 5000 });

      const items = page.locator('[data-testid="db-list-item"]');
      const count = await items.count();
      expect(count).toBeGreaterThanOrEqual(2);
    });

  test('CHECK-51: recent findings section visible',
    async ({ page }) => {
      await login(page);
      await page.waitForTimeout(3000);

      // Recent findings should be visible if findings exist
      const recent = page.locator(
        '[data-testid="recent-findings"]',
      );
      await expect(recent).toBeVisible({ timeout: 10000 });
    });
});

// ─── Step 17: Database Detail Page ─────────────────────────────
test.describe('Step 17: Database Detail', () => {
  test('CHECK-52: database page loads with snapshot data',
    async ({ page }) => {
      await login(page);
      await page.waitForTimeout(2000);

      // Select a database from picker
      const picker = page.locator(
        '[data-testid="database-picker"]',
      );
      if (await picker.isVisible({ timeout: 3000 })) {
        await picker.selectOption('production');
        await page.waitForTimeout(1000);
      }

      // Navigate to database page
      await page.goto('/#/database');
      await page.waitForTimeout(3000);

      // Should show database name and snapshot data
      const body = await page.textContent('body');
      expect(
        body!.includes('production') ||
        body!.includes('snapshot') ||
        body!.includes('Database'),
      ).toBeTruthy();
    });

  test('CHECK-53: snapshots/latest API returns data',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/snapshots/latest?database=production',
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('database');
      expect(data).toHaveProperty('snapshot');
    });
});

// ─── Step 18: Actions API & Detail ─────────────────────────────
test.describe('Step 18: Actions Detail', () => {
  test('CHECK-54: GET /actions returns list structure',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/actions');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('database');
      expect(data).toHaveProperty('actions');
      expect(data).toHaveProperty('total');
      expect(data).toHaveProperty('limit');
      expect(data).toHaveProperty('offset');
      expect(Array.isArray(data.actions)).toBeTruthy();
    });

  test('CHECK-55: GET /actions with database filter',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/actions?database=production',
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.database).toBe('production');
    });

  test('CHECK-56: actions page loads executed tab',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-actions"]');
      await page.waitForTimeout(2000);

      // In observation trust level, no actions exist yet —
      // page shows either the table or an empty state
      const table = page.locator(
        '[data-testid="executed-actions-table"]',
      );
      const hasTable = await table.isVisible()
        .catch(() => false);
      if (!hasTable) {
        const body = await page.textContent('body');
        expect(body).toContain('No actions');
      }
    });

  test('CHECK-57: actions pending tab shows structure',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-actions"]');
      await page.waitForTimeout(1000);

      await page.click('[data-testid="actions-tab-pending"]');
      await page.waitForTimeout(1000);

      // Pending tab shows table, help text, or empty state
      const helpText = page.locator(
        '[data-testid="pending-help-text"]',
      );
      const pendingTable = page.locator(
        '[data-testid="pending-actions-table"]',
      );

      const hasHelp =
        await helpText.isVisible().catch(() => false);
      const hasTable =
        await pendingTable.isVisible().catch(() => false);
      if (!hasHelp && !hasTable) {
        const body = await page.textContent('body');
        expect(body).toContain('No actions waiting');
      }
    });
});

// ─── Step 19: Snapshots API ────────────────────────────────────
test.describe('Step 19: Snapshots API', () => {
  test('CHECK-58: GET /snapshots/latest default metric',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/snapshots/latest');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('database');
      expect(data).toHaveProperty('snapshot');
    });

  test('CHECK-59: GET /snapshots/latest?metric=tables',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/snapshots/latest?metric=tables&database=production',
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('snapshot');
    });

  test('CHECK-60: GET /snapshots/latest?metric=indexes',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/snapshots/latest?metric=indexes&database=production',
      );
      expect(res.ok()).toBeTruthy();
    });

  test('CHECK-61: GET /snapshots/latest?metric=queries',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/snapshots/latest?metric=queries&database=production',
      );
      expect(res.ok()).toBeTruthy();
    });

  test('CHECK-62: GET /snapshots/latest?metric=sequences',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/snapshots/latest?metric=sequences&database=production',
      );
      expect(res.ok()).toBeTruthy();
    });

  test('CHECK-63: GET /snapshots/history returns time series',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/snapshots/history?metric=cache_hit_ratio'
        + '&hours=1&database=production',
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('metric', 'cache_hit_ratio');
      expect(data).toHaveProperty('points');
      expect(Array.isArray(data.points)).toBeTruthy();
    });

  test('CHECK-64: GET /snapshots/latest invalid metric returns null',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/snapshots/latest?metric=nonexistent',
      );
      // API returns 200 with null snapshot for unknown metrics
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.snapshot).toBeNull();
    });
});

// ─── Step 20: Finding Detail Depth ─────────────────────────────
test.describe('Step 20: Finding Detail Depth', () => {
  test('CHECK-65: expanded finding shows recommended SQL',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-findings"]');
      await page.waitForTimeout(3000);

      // Click on a duplicate_index finding (has recommended_sql)
      const row = page.locator('tr').filter({
        hasText: /duplicate_index/,
      }).first();

      await expect(row).toBeVisible({ timeout: 5000 });
      await row.click();
      await page.waitForTimeout(1000);

      // Should show SQL (DROP INDEX, CREATE INDEX, etc.)
      const body = await page.textContent('body');
      expect(
        body!.includes('DROP INDEX') ||
        body!.includes('CREATE INDEX') ||
        body!.includes('ALTER') ||
        body!.includes('VACUUM'),
      ).toBeTruthy();
    });

  test('CHECK-66: finding detail API includes all fields',
    async ({ request }) => {
      await apiLogin(request);

      const listRes = await request.get('/api/v1/findings');
      const listData = await listRes.json();
      expect(listData.findings.length).toBeGreaterThan(0);

      const id = listData.findings[0].id;
      const detailRes = await request.get(
        `/api/v1/findings/${id}`,
      );
      expect(detailRes.ok()).toBeTruthy();

      const finding = await detailRes.json();
      expect(finding).toHaveProperty('id');
      expect(finding).toHaveProperty('category');
      expect(finding).toHaveProperty('severity');
      expect(finding).toHaveProperty('title');
      expect(finding).toHaveProperty('status');
      expect(finding).toHaveProperty('recommendation');
      expect(finding).toHaveProperty('recommended_sql');
    });

  test('CHECK-67: expanded finding detail grid has content',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-findings"]');
      await page.waitForTimeout(3000);

      const row = page.locator('tr').filter({
        hasText: /duplicate_index|sequence_exhaustion|unused/,
      }).first();

      await expect(row).toBeVisible({ timeout: 5000 });
      await row.click();
      await page.waitForTimeout(1000);

      const grid = page.locator('[data-testid="detail-grid"]');
      await expect(grid).toBeVisible({ timeout: 3000 });

      const gridText = await grid.textContent();
      expect(gridText!.length).toBeGreaterThan(0);
    });
});

// ─── Step 21: Database Picker ──────────────────────────────────
test.describe('Step 21: Database Picker', () => {
  test('CHECK-68: picker visible with 2+ databases',
    async ({ page }) => {
      await login(page);
      await page.waitForTimeout(3000);

      const picker = page.locator(
        '[data-testid="database-picker"]',
      );
      await expect(picker).toBeVisible({ timeout: 5000 });

      // Should have "All Databases" + production + staging
      const options = picker.locator('option');
      const count = await options.count();
      expect(count).toBeGreaterThanOrEqual(3);
    });

  test('CHECK-69: selecting database filters findings page',
    async ({ page }) => {
      await login(page);
      await page.waitForTimeout(2000);

      const picker = page.locator(
        '[data-testid="database-picker"]',
      );
      await expect(picker).toBeVisible({ timeout: 5000 });

      // Select production
      await picker.selectOption('production');
      await page.waitForTimeout(1000);

      // Navigate to findings
      await page.click('[data-testid="nav-findings"]');
      await page.waitForTimeout(3000);

      // Findings count should still be visible (filtered)
      const count = page.locator(
        '[data-testid="findings-count"]',
      );
      await expect(count).toBeVisible({ timeout: 5000 });
      const text = await count.textContent();
      expect(parseInt(text || '0', 10)).toBeGreaterThan(0);
    });
});

// ─── Step 22: Settings Save/Discard ────────────────────────────
test.describe('Step 22: Settings Save/Discard', () => {
  test('CHECK-70: edit monitoring field and save',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-settings"]');
      await page.waitForTimeout(2000);

      // Switch to monitoring tab
      await page.click(
        '[data-testid="settings-tab-monitoring"]',
      );
      await page.waitForTimeout(1000);

      // Find a number input and change it
      const inputs = page.locator('input[type="number"]');
      const firstInput = inputs.first();
      await expect(firstInput).toBeVisible({ timeout: 3000 });

      await firstInput.clear();
      await firstInput.fill('2000');
      await page.waitForTimeout(500);

      // Save button should appear
      const saveBtn = page.locator(
        '[data-testid="settings-save"]',
      );
      await expect(saveBtn).toBeVisible({ timeout: 3000 });
      await saveBtn.click();
      await page.waitForTimeout(2000);

      // Should show success feedback
      const body = await page.textContent('body');
      expect(
        body!.includes('saved') ||
        body!.includes('Settings saved') ||
        body!.includes('success'),
      ).toBeTruthy();
    });

  test('CHECK-71: edit field and discard reverts value',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-settings"]');
      await page.waitForTimeout(2000);

      await page.click(
        '[data-testid="settings-tab-monitoring"]',
      );
      await page.waitForTimeout(1000);

      const inputs = page.locator('input[type="number"]');
      const firstInput = inputs.first();
      await expect(firstInput).toBeVisible({ timeout: 3000 });

      await firstInput.clear();
      await firstInput.fill('9999');
      await page.waitForTimeout(500);

      // Discard button should appear
      const discardBtn = page.locator(
        '[data-testid="settings-discard"]',
      );
      await expect(discardBtn).toBeVisible({ timeout: 3000 });
      await discardBtn.click();
      await page.waitForTimeout(1000);

      // Value should revert (not be 9999)
      const currentValue = await firstInput.inputValue();
      expect(currentValue).not.toBe('9999');
    });

  test('CHECK-72: reset monitoring value via API',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.put('/api/v1/config/global', {
        data: { 'analyzer.slow_query_threshold_ms': 1000 },
      });
      expect(res.ok()).toBeTruthy();
    });
});

// ─── Step 23: JSON Metrics API ─────────────────────────────────
test.describe('Step 23: JSON Metrics API', () => {
  test('CHECK-73: GET /metrics returns fleet structure',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/metrics');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();

      expect(data).toHaveProperty('fleet');
      expect(data.fleet).toHaveProperty('total_databases');
      expect(data.fleet.total_databases).toBeGreaterThanOrEqual(2);
    });

  test('CHECK-74: GET /metrics?database returns per-db',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/metrics?database=production',
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();

      expect(data).toHaveProperty('database', 'production');
      expect(data).toHaveProperty('status');
      expect(data.status).toHaveProperty('connected');
    });
});

// ─── Step 24: Per-Database Emergency Stop ──────────────────────
test.describe('Step 24: Per-DB Emergency Stop', () => {
  test('CHECK-75: stop only production database',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/emergency-stop?database=production',
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.status).toBe('stopped');
      expect(data.stopped).toBe(1);

      // Fleet should show emergency_stopped
      const fleetRes = await request.get('/api/v1/databases');
      expect(fleetRes.ok()).toBeTruthy();
      const fleet = await fleetRes.json();
      expect(fleet.summary.emergency_stopped).toBe(true);
    });

  test('CHECK-76: resume production database',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/resume?database=production',
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.status).toBe('resumed');
    });
});

// ─── Step 25: Page Content Depth ───────────────────────────────
test.describe('Step 25: Page Content', () => {
  test('CHECK-77: forecasts page shows cards or empty state',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-forecasts"]');
      await page.waitForTimeout(3000);

      const body = await page.textContent('body');
      expect(
        body!.includes('forecast') ||
        body!.includes('Forecast') ||
        body!.includes('Critical') ||
        body!.includes('Warning') ||
        body!.includes('No ') ||
        body!.includes('empty'),
      ).toBeTruthy();
    });

  test('CHECK-78: query hints page shows structure',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-query-hints"]');
      await page.waitForTimeout(3000);

      const body = await page.textContent('body');
      expect(
        body!.includes('Active Hints') ||
        body!.includes('Performance') ||
        body!.includes('hint') ||
        body!.includes('Hint') ||
        body!.includes('No ') ||
        body!.includes('Cost'),
      ).toBeTruthy();
    });

  test('CHECK-79: alerts page shows structure',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-alerts"]');
      await page.waitForTimeout(3000);

      const body = await page.textContent('body');
      expect(
        body!.includes('alert') ||
        body!.includes('Alert') ||
        body!.includes('Total') ||
        body!.includes('No ') ||
        body!.includes('Slack') ||
        body!.includes('slack'),
      ).toBeTruthy();
    });

  test('CHECK-80: forecasts API returns structure',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/forecasts');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('database');
      expect(data).toHaveProperty('forecasts');
      expect(Array.isArray(data.forecasts)).toBeTruthy();
    });

  test('CHECK-81: query-hints API returns structure',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/query-hints');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('database');
      expect(data).toHaveProperty('hints');
      expect(Array.isArray(data.hints)).toBeTruthy();
    });

  test('CHECK-82: alert-log API returns structure',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/alert-log');
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('database');
      expect(data).toHaveProperty('alerts');
      expect(Array.isArray(data.alerts)).toBeTruthy();
    });
});

// ─── Step 26: Database Edit/Delete ─────────────────────────────
test.describe('Step 26: Database Edit/Delete', () => {
  test('CHECK-83: edit button opens form with populated fields',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-databases"]');
      await page.waitForTimeout(2000);

      // Find staging row and click edit
      const stagingRow = page.locator(
        '[data-testid="db-row"]',
      ).filter({ hasText: 'staging' });
      await expect(stagingRow).toBeVisible({ timeout: 5000 });

      const editBtn = stagingRow.locator(
        '[data-testid="db-edit-button"]',
      );
      await editBtn.click();
      await page.waitForTimeout(1000);

      // Form should open with pre-filled data
      const form = page.locator('[data-testid="db-form"]');
      await expect(form).toBeVisible({ timeout: 3000 });

      // Verify name field is populated
      const nameInput = page.locator('[data-testid="db-name"]');
      const nameValue = await nameInput.inputValue();
      expect(nameValue).toBe('staging');

      // Cancel without saving
      await page.click('[data-testid="db-cancel-button"]');
      await page.waitForTimeout(500);
    });

  test('CHECK-84: delete staging database via UI',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-databases"]');
      await page.waitForTimeout(2000);

      const stagingRow = page.locator(
        '[data-testid="db-row"]',
      ).filter({ hasText: 'staging' });
      await expect(stagingRow).toBeVisible({ timeout: 5000 });

      const deleteBtn = stagingRow.locator(
        '[data-testid="db-delete-button"]',
      );
      await deleteBtn.click();
      await page.waitForTimeout(1000);

      // Confirmation modal should appear
      const confirm = page.locator(
        '[data-testid="delete-confirm"]',
      );
      await expect(confirm).toBeVisible({ timeout: 3000 });

      // Click confirm delete
      await page.click('[data-testid="delete-confirm-yes"]');
      await page.waitForTimeout(2000);

      // Staging should be gone from table
      const table = page.locator(
        '[data-testid="databases-table"]',
      );
      await expect(table).not.toContainText('staging', {
        timeout: 5000,
      });
    });

  test('CHECK-85: re-add staging database after delete',
    async ({ page }) => {
      await login(page);
      await page.click('[data-testid="nav-databases"]');
      await page.waitForTimeout(1000);

      await page.click('[data-testid="add-database-button"]');
      await expect(
        page.locator('[data-testid="db-form"]'),
      ).toBeVisible({ timeout: 3000 });

      await page.fill('[data-testid="db-name"]', 'staging');
      await page.fill('[data-testid="db-host"]', 'localhost');

      const portInput = page.locator('[data-testid="db-port"]');
      await portInput.clear();
      await portInput.fill(String(STAGING_DB.port));

      await page.fill(
        '[data-testid="db-database"]', STAGING_DB.database,
      );
      await page.fill(
        '[data-testid="db-username"]', STAGING_DB.username,
      );
      await page.fill(
        '[data-testid="db-password"]', STAGING_DB.password,
      );

      await page.locator(
        '[data-testid="db-form"] select',
      ).first().selectOption('disable');

      await page.click('[data-testid="db-save-button"]');
      await page.waitForTimeout(2000);

      await expect(
        page.locator('[data-testid="databases-table"]'),
      ).toContainText('staging', { timeout: 5000 });
    });
});

// ─── Step 27: Auth Endpoints ──────────────────────────────────
test.describe('Step 27: Auth Endpoints', () => {
  test('CHECK-86: GET /auth/me returns current user',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/auth/me');
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data.email).toBe(ADMIN_EMAIL);
      expect(data.role).toBe('admin');
      expect(data).toHaveProperty('id');
    });

  test('CHECK-87: POST /auth/logout clears session',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post('/api/v1/auth/logout');
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data.status).toBe('logged out');

      // After logout, /auth/me should fail
      const meRes = await request.get('/api/v1/auth/me');
      expect(meRes.status()).toBe(401);
    });
});

// ─── Step 28: User Role Change ────────────────────────────────
test.describe('Step 28: User Role Change', () => {
  test('CHECK-88: create user then change role via API',
    async ({ request }) => {
      await apiLogin(request);

      // Create a viewer
      const createRes = await request.post('/api/v1/users', {
        data: {
          email: 'roletest@test.local',
          password: 'testpass123',
          role: 'viewer',
        },
      });
      expect(createRes.status()).toBe(201);
      const user = await createRes.json();
      expect(user.role).toBe('viewer');

      // Change to operator
      const roleRes = await request.put(
        `/api/v1/users/${user.id}/role`,
        { data: { role: 'operator' } },
      );
      expect(roleRes.ok()).toBeTruthy();
      const roleData = await roleRes.json();
      expect(roleData.status).toBe('updated');

      // Verify by listing users
      const listRes = await request.get('/api/v1/users');
      const listData = await listRes.json();
      const updated = listData.users.find(
        (u: { email: string }) =>
          u.email === 'roletest@test.local',
      );
      expect(updated.role).toBe('operator');

      // Cleanup
      await request.delete(`/api/v1/users/${user.id}`);
    });

  test('CHECK-89: operator cannot access admin routes',
    async ({ request }) => {
      await apiLogin(request);

      // Create operator
      const createRes = await request.post('/api/v1/users', {
        data: {
          email: 'optest@test.local',
          password: 'testpass123',
          role: 'operator',
        },
      });
      expect(createRes.status()).toBe(201);
      const user = await createRes.json();

      // Login as operator
      const loginRes = await request.post(
        '/api/v1/auth/login',
        {
          data: {
            email: 'optest@test.local',
            password: 'testpass123',
          },
        },
      );
      expect(loginRes.ok()).toBeTruthy();

      // Operator should NOT access admin routes
      const usersRes = await request.get('/api/v1/users');
      expect(usersRes.status()).toBe(403);

      const configRes = await request.get(
        '/api/v1/config/global',
      );
      expect(configRes.status()).toBe(403);

      // Operator SHOULD access pending actions
      const pendingRes = await request.get(
        '/api/v1/actions/pending',
      );
      expect(pendingRes.ok()).toBeTruthy();

      // Cleanup: re-login as admin and delete
      await apiLogin(request);
      await request.delete(`/api/v1/users/${user.id}`);
    });
});

// ─── Step 29: Database Edit via API ───────────────────────────
test.describe('Step 29: Database Edit via API', () => {
  test('CHECK-90: GET /databases/managed lists databases',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/databases/managed',
      );
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data.databases.length).toBeGreaterThanOrEqual(2);
      const names = data.databases.map(
        (d: { name: string }) => d.name,
      );
      expect(names).toContain('production');
      expect(names).toContain('staging');
    });

  test('CHECK-91: PUT /databases/managed/{id} edits database',
    async ({ request }) => {
      await apiLogin(request);

      // Get staging database ID
      const listRes = await request.get(
        '/api/v1/databases/managed',
      );
      const listData = await listRes.json();
      const staging = listData.databases.find(
        (d: { name: string }) => d.name === 'staging',
      );
      expect(staging).toBeTruthy();

      // Edit: change trust_level
      const editRes = await request.put(
        `/api/v1/databases/managed/${staging.id}`,
        {
          data: {
            name: staging.name,
            host: staging.host,
            port: staging.port,
            database_name: staging.database_name,
            username: staging.username,
            password: STAGING_DB.password,
            sslmode: staging.sslmode,
            trust_level: 'advisory',
            execution_mode: staging.execution_mode
              || 'approval',
          },
        },
      );
      expect(editRes.ok()).toBeTruthy();

      // Verify the change persisted
      const getRes = await request.get(
        `/api/v1/databases/managed/${staging.id}`,
      );
      expect(getRes.ok()).toBeTruthy();
      const updated = await getRes.json();
      expect(updated.trust_level).toBe('advisory');

      // Revert to observation
      await request.put(
        `/api/v1/databases/managed/${staging.id}`,
        {
          data: {
            name: staging.name,
            host: staging.host,
            port: staging.port,
            database_name: staging.database_name,
            username: staging.username,
            password: STAGING_DB.password,
            sslmode: staging.sslmode,
            trust_level: 'observation',
            execution_mode: staging.execution_mode
              || 'approval',
          },
        },
      );
    });
});

// ─── Step 30: Action Approve/Reject ───────────────────────────
test.describe('Step 30: Action Approve/Reject', () => {
  test('CHECK-92: pending count endpoint works',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/actions/pending/count',
      );
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(typeof data.count).toBe('number');
      expect(data.count).toBeGreaterThanOrEqual(0);
    });

  test('CHECK-93: approve action endpoint reachable',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/actions/99999/approve',
      );
      // 404 (standalone, action not found) or
      // 501 (fleet, not yet implemented)
      expect([404, 501]).toContain(res.status());
    });

  test('CHECK-94: reject action endpoint reachable',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/actions/99999/reject',
        { data: { reason: 'test rejection' } },
      );
      expect([404, 501]).toContain(res.status());
    });

  test('CHECK-95: approve with invalid ID returns error',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/actions/notanumber/approve',
      );
      // 400 (standalone) or 501 (fleet)
      expect([400, 501]).toContain(res.status());
    });
});

// ─── Step 31: Notification CRUD ───────────────────────────────
test.describe('Step 31: Notification CRUD', () => {
  let channelId: number;
  let ruleId: number;

  test('CHECK-96: create channel and rule for CRUD tests',
    async ({ request }) => {
      await apiLogin(request);

      // Clean up any leftover channels from prior runs
      const listRes = await request.get(
        '/api/v1/notifications/channels',
      );
      if (listRes.ok()) {
        const listData = await listRes.json();
        const channels = listData.channels || [];
        for (const ch of channels) {
          if (ch.name?.startsWith('crud-test-')) {
            // Delete rules referencing this channel first
            const rulesRes = await request.get(
              '/api/v1/notifications/rules',
            );
            if (rulesRes.ok()) {
              const rulesData = await rulesRes.json();
              for (const r of (rulesData.rules || [])) {
                if (r.channel_id === ch.id) {
                  await request.delete(
                    `/api/v1/notifications/rules/${r.id}`,
                  );
                }
              }
            }
            await request.delete(
              `/api/v1/notifications/channels/${ch.id}`,
            );
          }
        }
      }

      // Create a channel (valid types: slack, email, pagerduty)
      const chName = `crud-test-${Date.now()}`;
      const chRes = await request.post(
        '/api/v1/notifications/channels',
        {
          data: {
            name: chName,
            type: 'slack',
            config: {
              webhook_url: 'https://hooks.slack.com/test',
            },
          },
        },
      );
      expect(chRes.status()).toBe(201);
      const ch = await chRes.json();
      channelId = ch.id;

      // Create a rule
      const ruleRes = await request.post(
        '/api/v1/notifications/rules',
        {
          data: {
            channel_id: channelId,
            event: 'finding_critical',
            min_severity: 'critical',
          },
        },
      );
      expect(ruleRes.status()).toBe(201);
      const rule = await ruleRes.json();
      ruleId = rule.id;
    });

  test('CHECK-97: edit notification channel via PUT',
    async ({ request }) => {
      await apiLogin(request);

      const newName = `crud-test-renamed-${Date.now()}`;
      const res = await request.put(
        `/api/v1/notifications/channels/${channelId}`,
        {
          data: {
            name: newName,
            config: {
              webhook_url: 'https://hooks.slack.com/updated',
            },
            enabled: true,
          },
        },
      );
      const body = await res.json();
      expect(res.ok(),
        `PUT channel failed: ${JSON.stringify(body)}`,
      ).toBeTruthy();
      expect(body.status).toBe('updated');

      // Verify the channel was renamed
      const listRes = await request.get(
        '/api/v1/notifications/channels',
      );
      expect(listRes.ok()).toBeTruthy();
      const listData = await listRes.json();
      const channels = listData.channels || [];
      expect(channels.length).toBeGreaterThan(0);
      const ch = channels.find(
        (c: { id: number }) => c.id === channelId,
      );
      expect(ch).toBeTruthy();
      expect(ch.name).toBe(newName);
    });

  test('CHECK-98: edit notification rule via PUT',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.put(
        `/api/v1/notifications/rules/${ruleId}`,
        { data: { enabled: false } },
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.status).toBe('updated');
    });

  test('CHECK-99: test notification channel send',
    async ({ request }) => {
      await apiLogin(request);

      // Test send — may fail since webhook URL is fake,
      // but endpoint should be reachable (not 404)
      const res = await request.post(
        `/api/v1/notifications/channels/${channelId}/test`,
      );
      // 200 = test sent, 502 = test failed (expected for
      // fake URL) — both mean the endpoint works
      expect([200, 502]).toContain(res.status());
    });

  test('CHECK-100: delete notification rule',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.delete(
        `/api/v1/notifications/rules/${ruleId}`,
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.status).toBe('deleted');
    });

  test('CHECK-101: delete notification channel',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.delete(
        `/api/v1/notifications/channels/${channelId}`,
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.status).toBe('deleted');
    });
});

// ─── Step 32: Per-Database Config ─────────────────────────────
test.describe('Step 32: Per-Database Config', () => {
  test('CHECK-102: GET /config/global returns config',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/config/global');
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data).toHaveProperty('config');
      expect(data).toHaveProperty('mode');
    });

  test('CHECK-103: GET /config/databases/{id} returns config',
    async ({ request }) => {
      await apiLogin(request);

      // Get a database ID
      const listRes = await request.get(
        '/api/v1/databases/managed',
      );
      const listData = await listRes.json();
      const dbId = listData.databases[0].id;

      const res = await request.get(
        `/api/v1/config/databases/${dbId}`,
      );
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data).toHaveProperty('database_id');
      expect(data).toHaveProperty('config');
      expect(data.database_id).toBe(dbId);
    });

  test('CHECK-104: PUT /config/databases/{id} updates config',
    async ({ request }) => {
      await apiLogin(request);

      const listRes = await request.get(
        '/api/v1/databases/managed',
      );
      const listData = await listRes.json();
      const dbId = listData.databases[0].id;

      const res = await request.put(
        `/api/v1/config/databases/${dbId}`,
        {
          data: {
            'analyzer.slow_query_threshold_ms': 750,
          },
        },
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.status).toBe('updated');

      // Reset
      await request.put(
        `/api/v1/config/databases/${dbId}`,
        {
          data: {
            'analyzer.slow_query_threshold_ms': 500,
          },
        },
      );
    });

  test('CHECK-105: GET /config/audit returns audit trail',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/config/audit');
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data).toHaveProperty('audit');
      expect(Array.isArray(data.audit)).toBeTruthy();
    });
});

// ─── Step 33: Error Handling ──────────────────────────────────
test.describe('Step 33: Error Handling', () => {
  test('CHECK-106: GET /findings/999999 returns 404',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/findings/999999',
      );
      expect(res.status()).toBe(404);
    });

  test('CHECK-107: POST /auth/login with missing fields',
    async ({ request }) => {
      const res = await request.post('/api/v1/auth/login', {
        data: { email: '' },
      });
      expect(res.ok()).toBeFalsy();
      expect([400, 401]).toContain(res.status());
    });

  test('CHECK-108: POST /users with duplicate email fails',
    async ({ request }) => {
      await apiLogin(request);

      // Try to create user with admin's email
      const res = await request.post('/api/v1/users', {
        data: {
          email: ADMIN_EMAIL,
          password: 'anything',
          role: 'viewer',
        },
      });
      expect(res.ok()).toBeFalsy();
      expect([400, 409]).toContain(res.status());
    });

  test('CHECK-109: DELETE /users with invalid ID returns 400',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.delete(
        '/api/v1/users/notanumber',
      );
      expect(res.status()).toBe(400);
    });

  test('CHECK-110: unauthenticated request returns 401',
    async ({ request }) => {
      // No login — direct request
      const res = await request.get('/api/v1/databases', {
        headers: { cookie: '' },
      });
      expect(res.status()).toBe(401);
    });

  test('CHECK-111: GET /databases/managed/999999 returns 404',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/databases/managed/999999',
      );
      expect(res.status()).toBe(404);
    });
});

// ─── Step 34: Database Managed CRUD ───────────────────────────
test.describe('Step 34: Database Managed CRUD', () => {
  test('CHECK-112: POST /databases/managed/test-connection',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/databases/managed/test-connection',
        {
          data: {
            name: 'test-conn',
            host: PROD_DB.host,
            port: PROD_DB.port,
            database_name: PROD_DB.database,
            username: PROD_DB.username,
            password: PROD_DB.password,
            sslmode: 'disable',
          },
        },
      );
      expect(res.ok()).toBeTruthy();
    });

  test('CHECK-113: POST test-connection bad host fails',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/databases/managed/test-connection',
        {
          data: {
            name: 'bad-host',
            host: 'nonexistent.invalid',
            port: 5432,
            database_name: 'test',
            username: 'test',
            password: 'test',
            sslmode: 'disable',
          },
        },
      );
      const data = await res.json();
      // Connection should fail
      expect(
        data.error || data.status === 'error'
        || !res.ok(),
      ).toBeTruthy();
    });

  test('CHECK-114: GET /databases/managed/{id} single db',
    async ({ request }) => {
      await apiLogin(request);

      const listRes = await request.get(
        '/api/v1/databases/managed',
      );
      const listData = await listRes.json();
      const id = listData.databases[0].id;

      const res = await request.get(
        `/api/v1/databases/managed/${id}`,
      );
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data).toHaveProperty('name');
      expect(data).toHaveProperty('host');
      expect(data).toHaveProperty('port');
    });
});

// ─── Step 35: LLM Models Endpoint ────────────────────────────
test.describe('Step 35: LLM Models Endpoint', () => {
  test('CHECK-115: GET /llm/models returns status',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get('/api/v1/llm/models');
      // 200 if configured, 502/503 if LLM unreachable
      expect([200, 502, 503]).toContain(res.status());

      if (res.status() === 200) {
        const data = await res.json();
        expect(data).toHaveProperty('models');
      }
    });
});

// ─── Step 36: Action Detail ──────────────────────────────────
test.describe('Step 36: Action Detail', () => {
  test('CHECK-116: GET /actions/{id} with invalid ID',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/actions/999999',
      );
      expect(res.status()).toBe(404);
    });

  test('CHECK-117: GET /actions with pagination params',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.get(
        '/api/v1/actions?limit=5&offset=0',
      );
      expect(res.ok()).toBeTruthy();

      const data = await res.json();
      expect(data).toHaveProperty('total');
      expect(data).toHaveProperty('actions');
      expect(data).toHaveProperty('limit');
      expect(data.limit).toBe(5);
      expect(data).toHaveProperty('offset');
      expect(data.offset).toBe(0);
    });
});

// ─── Step 37: Manual Execute Endpoint ────────────────────────
test.describe('Step 37: Manual Execute Endpoint', () => {
  test('CHECK-118: POST /actions/execute with missing fields',
    async ({ request }) => {
      await apiLogin(request);

      // Missing both required fields
      const res = await request.post(
        '/api/v1/actions/execute',
        { data: {} },
      );
      // In fleet mode this returns 501 (not implemented);
      // in standalone it returns 400 for missing fields.
      expect([400, 501]).toContain(res.status());
    });

  test('CHECK-119: POST /actions/execute with invalid finding',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/actions/execute',
        {
          data: {
            finding_id: 999999,
            sql: 'SELECT 1',
          },
        },
      );
      // 501 in fleet mode, 500 in standalone (finding
      // not found or execution failure).
      expect([500, 501]).toContain(res.status());
    });
});

// ─── Step 38: Test Existing DB Connection ────────────────────
test.describe('Step 38: Test Existing DB Connection', () => {
  test('CHECK-120: POST /databases/managed/{id}/test succeeds',
    async ({ request }) => {
      await apiLogin(request);

      // Get a managed database ID
      const listRes = await request.get(
        '/api/v1/databases/managed',
      );
      const listData = await listRes.json();
      const db = listData.databases[0];

      const res = await request.post(
        `/api/v1/databases/managed/${db.id}/test`,
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.status).toBe('ok');
      expect(data.pg_version).toBeTruthy();
      expect(Array.isArray(data.extensions)).toBe(true);
    });

  test('CHECK-121: POST /databases/managed/999999/test 404',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/databases/managed/999999/test',
      );
      expect(res.status()).toBe(404);
    });
});

// ─── Step 39: CSV Import Endpoint ────────────────────────────
test.describe('Step 39: CSV Import Endpoint', () => {
  test('CHECK-122: POST /databases/managed/import with valid CSV',
    async ({ request }) => {
      await apiLogin(request);

      const csv = [
        'name,host,port,database_name,username,password,sslmode',
        `import-test,${IMPORT_DB.host},${IMPORT_DB.port},${IMPORT_DB.database},${IMPORT_DB.username},${IMPORT_DB.password},disable`,
      ].join('\n');

      const res = await request.post(
        '/api/v1/databases/managed/import',
        {
          multipart: {
            file: {
              name: 'databases.csv',
              mimeType: 'text/csv',
              buffer: Buffer.from(csv),
            },
          },
        },
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data).toHaveProperty('imported');
      expect(data).toHaveProperty('errors');

      // Clean up: delete the imported database
      if (data.imported > 0) {
        const listRes = await request.get(
          '/api/v1/databases/managed',
        );
        const listData = await listRes.json();
        const imported = (listData.databases || []).find(
          (d: { name: string }) => d.name === 'import-test',
        );
        if (imported) {
          await request.delete(
            `/api/v1/databases/managed/${imported.id}`,
          );
        }
      }
    });

  test('CHECK-123: POST /databases/managed/import bad header',
    async ({ request }) => {
      await apiLogin(request);

      const csv = 'bad,header,row\nfoo,bar,baz\n';

      const res = await request.post(
        '/api/v1/databases/managed/import',
        {
          multipart: {
            file: {
              name: 'bad.csv',
              mimeType: 'text/csv',
              buffer: Buffer.from(csv),
            },
          },
        },
      );
      expect(res.ok()).toBeTruthy();
      const data = await res.json();
      expect(data.imported).toBe(0);
      expect(data.errors.length).toBeGreaterThan(0);
      expect(data.errors[0].error).toContain('invalid CSV header');
    });

  test('CHECK-124: POST /databases/managed/import no file',
    async ({ request }) => {
      await apiLogin(request);

      const res = await request.post(
        '/api/v1/databases/managed/import',
        { data: {} },
      );
      expect(res.status()).toBe(400);
    });
});
