import { test, expect } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const ADMIN_EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const ADMIN_PASS = process.env.PG_SAGE_ADMIN_PASS || 'admin';

test.describe('Databases (admin)', () => {
  let consoleErrors: string[];

  test.beforeEach(async ({ page }) => {
    consoleErrors = getConsoleErrors(page);
    await login(page, ADMIN_EMAIL, ADMIN_PASS);
    await page.goto('/#/manage-databases');
  });

  test.afterEach(async () => {
    // getConsoleErrors already filters expected errors at collection time.
    expect(consoleErrors).toEqual([]);
  });

  // Verifies the databases management page loads for admin users
  test('databases page loads (admin only)', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    // The "Add Database" button should be visible
    const addBtn = page.locator('button:has-text("Add Database")');
    await expect(addBtn).toBeVisible();
  });

  // Verifies clicking "Add Database" opens the database form
  test('add database button opens form', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    const addBtn = page.locator('button:has-text("Add Database")');
    await addBtn.click();

    // The form should appear with a heading "Add Database"
    const formHeading = page.locator('h2:has-text("Add Database")');
    await expect(formHeading).toBeVisible();
  });

  // Verifies the database form contains all required connection fields
  test('database form has all required fields', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    const addBtn = page.locator('button:has-text("Add Database")');
    await addBtn.click();

    // Check for all field labels (use exact text match to avoid
    // "Name" matching "Username")
    const requiredLabels = [
      'Name', 'Host', 'Port', 'Database', 'Username',
      'Password', 'SSL Mode', 'Max Connections',
      'Trust Level', 'Execution Mode',
    ];
    for (const label of requiredLabels) {
      const labelEl = page.locator('label').filter({
        hasText: new RegExp(`^${label}$`),
      });
      await expect(labelEl).toBeVisible();
    }

    // Save and Cancel buttons should be present
    const saveBtn = page.locator(
      'form >> button[type="submit"]:has-text("Save")',
    );
    await expect(saveBtn).toBeVisible();

    const cancelBtn = page.locator(
      'form >> button:has-text("Cancel")',
    );
    await expect(cancelBtn).toBeVisible();
  });

  // Verifies the Cancel button closes the form
  test('cancel button closes form', async ({ page }) => {
    // Wait for page to render (proves API data loaded)
    await page.waitForSelector('button:has-text("Add Database")');

    const addBtn = page.locator('button:has-text("Add Database")');
    await addBtn.click();

    // Confirm the form is open
    const formHeading = page.locator('h2:has-text("Add Database")');
    await expect(formHeading).toBeVisible();

    // Click Cancel
    const cancelBtn = page.locator('form >> button:has-text("Cancel")');
    await cancelBtn.click();

    // The form heading should no longer be visible
    await expect(formHeading).not.toBeVisible();
  });

  test('add database shows encryption-key requirement when unavailable',
    async ({ page }, testInfo) => {
      await page.locator('[data-testid="add-database-button"]').click();

      const name = `codex-create-${testInfo.workerIndex}-${Date.now()}`;
      await page.locator('[data-testid="db-name"]').fill(name);
      await page.locator('[data-testid="db-host"]').fill('127.0.0.1');
      await page.locator('[data-testid="db-port"]').fill('5433');
      await page.locator('[data-testid="db-database"]').fill('testdb');
      await page.locator('[data-testid="db-username"]').fill('postgres');
      await page.locator('[data-testid="db-password"]').fill('test');
      await page.locator('[data-testid="db-sslmode"]').selectOption('disable');
      await page.locator('[data-testid="db-max-connections"]').fill('2');
      await page.locator('[data-testid="db-trust-level"]').selectOption(
        'advisory',
      );
      await page.locator('[data-testid="db-execution-mode"]').selectOption(
        'approval',
      );

      await Promise.all([
        page.waitForResponse((res) =>
          res.url().includes('/api/v1/databases/managed') &&
          res.request().method() === 'POST' &&
          res.status() === 400,
        ),
        page.locator('[data-testid="db-save-button"]').click(),
      ]);
      await expect(page.locator('main')).toContainText('encryption_key');
      consoleErrors = consoleErrors.filter(
        (msg) => !msg.includes('400 (Bad Request)'),
      );
    });

  test('edit form test connection uses unsaved fields and stored password',
    async ({ page }, testInfo) => {
      const listRes = await page.request.get('/api/v1/databases/managed');
      expect(listRes.status()).toBe(200);
      const listData = await listRes.json();
      const db = (listData.databases || []).find(
        (row: any) => row.name === 'testdb',
      ) || (listData.databases || [])[0];
      test.skip(!db, 'No managed databases are configured');

      const row = page.locator('[data-testid="db-row"]', {
        hasText: db.name,
      }).first();
      await expect(row).toBeVisible();
      await row.locator('[data-testid="db-edit-button"]').click();

      await expect(page.locator('[data-testid="db-form"]')).toBeVisible();
      await expect(page.locator('[data-testid="db-password"]'))
        .toHaveValue('');

      const missingDB =
        `codex_missing_${testInfo.workerIndex}_${Date.now()}`;
      await page.locator('[data-testid="db-database"]').fill(missingDB);
      await page.locator('[data-testid="db-test-connection"]').click();
      await expect(page.locator('[data-testid="db-form"]'))
        .toContainText('Error:');

      await page.locator('[data-testid="db-database"]').fill(
        db.database_name,
      );
      await page.locator('[data-testid="db-test-connection"]').click();
      await expect(page.locator('[data-testid="db-form"]'))
        .toContainText('Connected -');

      await page.locator('[data-testid="db-cancel-button"]').click();
      await expect(page.locator('[data-testid="db-form"]')).not.toBeVisible();
    });

  test('saving an edit preserves max connections and refreshes runtime',
    async ({ page }) => {
      const listRes = await page.request.get('/api/v1/databases/managed');
      expect(listRes.status()).toBe(200);
      const listData = await listRes.json();
      const db = (listData.databases || []).find(
        (row: any) => row.name === 'testdb',
      ) || (listData.databases || [])[0];
      test.skip(!db, 'No managed databases are configured');

      const originalTrust = db.trust_level;
      const newTrust = originalTrust === 'observation'
        ? 'advisory'
        : 'observation';
      const maxConnections = db.max_connections || 2;

      try {
        const row = page.locator('[data-testid="db-row"]', {
          hasText: db.name,
        }).first();
        await expect(row).toBeVisible();
        await row.locator('[data-testid="db-edit-button"]').click();

        await expect(page.locator('[data-testid="db-max-connections"]'))
          .toHaveValue(String(maxConnections));
        await page.locator('[data-testid="db-trust-level"]').selectOption(
          newTrust,
        );

        await Promise.all([
          page.waitForResponse((res) =>
            res.url().includes(`/api/v1/databases/managed/${db.id}`) &&
            res.request().method() === 'PUT' &&
            res.status() === 200,
          ),
          page.locator('[data-testid="db-save-button"]').click(),
        ]);
        await expect(page.locator('[data-testid="db-form"]')).not.toBeVisible();

        const afterList = await page.request.get('/api/v1/databases/managed');
        const afterData = await afterList.json();
        const afterDB = (afterData.databases || []).find(
          (row: any) => row.id === db.id,
        );
        expect(afterDB.max_connections).toBe(maxConnections);

        await expect.poll(async () => {
          const statusRes = await page.request.get('/api/v1/databases');
          const statusData = await statusRes.json();
          const statusDB = (statusData.databases || []).find(
            (row: any) => row.name === db.name,
          );
          return statusDB?.status?.trust_level;
        }, { timeout: 15000 }).toBe(newTrust);
      } finally {
        await page.request.put(`/api/v1/databases/managed/${db.id}`, {
          data: {
            name: db.name,
            host: db.host,
            port: db.port,
            database_name: db.database_name,
            username: db.username,
            password: '',
            sslmode: db.sslmode,
            max_connections: maxConnections,
            trust_level: originalTrust,
            execution_mode: db.execution_mode,
          },
        });
      }
    });
});
