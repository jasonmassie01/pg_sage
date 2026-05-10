import { test, expect } from '@playwright/test';
import { login } from './helpers';

const liveEnabled =
  process.env.PG_SAGE_LIVE_AWS_RDS === '1' ||
  process.env.PG_SAGE_LIVE_GCP_CLOUDSQL === '1' ||
  process.env.PG_SAGE_LIVE_DATABRICKS_LAKEBASE === '1';

test.describe('AgentDB live provisioning', () => {
  test.skip(!liveEnabled, 'live cloud provisioning requires explicit env flag');

  test('operator can reach Agent DB live provisioning surface', async ({ page }) => {
    await login(page, 'admin@pg-sage.local', 'pgSageQA!2026');
    await expect(page.locator('body')).toBeVisible();
    await page.getByTestId('nav-agent-dbs').click();
    await expect(page.getByTestId('agent-dbs-page')).toBeVisible({
      timeout: 30000,
    });
    await expect(page.getByText(/Provider Settings|Terraform|Provision/i).first()).toBeVisible();
  });
});
