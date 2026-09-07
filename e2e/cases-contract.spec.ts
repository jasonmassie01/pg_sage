import { test, expect, type Page } from '@playwright/test';
import { login, getConsoleErrors } from './helpers';

const EMAIL = process.env.PG_SAGE_ADMIN_EMAIL || 'admin@pg-sage.local';
const PASSWORD = process.env.PG_SAGE_ADMIN_PASS || 'admin';
const sources = ['finding', 'schema_health', 'query_hint', 'forecast', 'incident'];

async function stubCases(page: Page) {
  await page.route('**/api/v1/cases**', route => route.fulfill({
    json: { cases: sources.map((source, index) => ({
      case_id: `contract-${source}`, source_type: source, title: `${source} case`,
      severity: 'warning', state: 'open', impact_score: index / 10,
      evidence: [{ type: 'observation', summary: `${source} evidence` }],
    })) },
  }));
}

test.describe('Cases route contracts', () => {
  let errors: string[];
  test.beforeEach(async ({ page }) => {
    errors = getConsoleErrors(page);
    await login(page, EMAIL, PASSWORD);
    await stubCases(page);
  });
  test.afterEach(() => expect(errors).toEqual([]));

  test('source filters select only matching cards and restore all counts', async ({ page }) => {
    await page.goto('/#/cases');
    await expect(page.locator('main article')).toHaveCount(5);
    await page.getByRole('button', { name: 'Schema', exact: true }).click();
    await expect(page.locator('main article')).toHaveCount(1);
    await expect(page.locator('main article')).toContainText('schema_health evidence');
    await expect(page.getByTestId('cases-page-description')).toContainText('1 of 5');
    await page.getByRole('button', { name: 'All', exact: true }).click();
    await expect(page.locator('main article')).toHaveCount(5);
  });

  test('changing source routes resets the filter without reload', async ({ page }) => {
    await page.goto('/#/incidents');
    await expect(page.locator('main article')).toHaveText(/incident case/);
    await page.evaluate(() => { window.location.hash = '#/schema-health'; });
    await expect(page.getByRole('button', { name: 'Schema', exact: true }))
      .toHaveAttribute('aria-pressed', 'true');
    await expect(page.locator('main article')).toHaveText(/schema_health case/);
    await page.getByTestId('nav-cases').click();
    await expect(page.getByRole('button', { name: 'All', exact: true }))
      .toHaveAttribute('aria-pressed', 'true');
    await expect(page.locator('main article')).toHaveCount(5);
  });

  test('empty cases render an honest zero count', async ({ page }) => {
    await page.route('**/api/v1/cases**', route => route.fulfill({ json: { cases: [] } }));
    await page.goto('/#/cases');
    await expect(page.getByTestId('cases-page-description')).toContainText('0 of 0');
    await expect(page.locator('main article')).toHaveCount(0);
  });
});
