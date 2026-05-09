import { test, expect } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

test.describe('Agent DBs page', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
  })

  test('loads deployments, requests, cost, backups, and tuning hints', async ({
    page,
  }) => {
    await page.goto('#/agent-dbs')

    await expect(page.locator('[data-testid="agent-dbs-page"]')).toBeVisible()
    await expect(page.locator('[data-testid="agent-db-row"]')).toContainText(
      'agentdb-demo',
    )
    await expect(page.locator('[data-testid="agent-db-detail"]')).toContainText(
      'Shape pgvector queries',
    )
    await expect(page.locator('[data-testid="agent-db-detail"]')).toContainText(
      'Action: query_rewrite',
    )
    await expect(page.locator('[data-testid="agent-db-detail"]')).toContainText(
      'Risk: safe',
    )
    await expect(page.locator('[data-testid="agent-db-detail"]')).toContainText(
      'Confidence: 82%',
    )
    await expect(page.locator('[data-testid="agent-db-detail"]')).toContainText(
      'Instruction: add LIMIT before vector ORDER BY',
    )
    await expect(page.locator('[data-testid="agent-db-audit"]')).toContainText(
      'register',
    )
    await expect(page.locator('[data-testid="agent-db-audit"]')).toContainText(
      'recommendation_feedback',
    )
    await expect(page.locator('[data-testid="agent-db-promotion"]'))
      .toContainText('Promote agent schema')
    await expect(page.locator('[data-testid="agent-db-promotion"]'))
      .toContainText('review_only: true')
    await expect(page.locator('[data-testid="agent-db-detail"]')).toContainText(
      '$3.45',
    )
    await expect(page.locator('[data-testid="agent-db-requests"]')).toContainText(
      'req-agentdb-demo',
    )
    await expect(page.locator('[data-testid="agent-db-providers"]')).toContainText(
      'Databricks Lakebase',
    )
    await expect(page.locator('[data-testid="agent-db-profiles"]')).toContainText(
      'Lakebase instance S',
    )
    await expect(page.locator('[data-testid="agent-db-provisioning"]'))
      .toContainText('Cloud provisioning')
    await expect(page.locator('[data-testid="agent-db-provisioning"]'))
      .toContainText('dry run: databricks database branches create')
    await expect(page.locator('[data-testid="agent-db-backup-assurance"]'))
      .toContainText('restore_verified')
  })

  test('provisions through the UI request path', async ({ page }) => {
    await page.goto('#/agent-dbs')

    await page.locator('[data-testid="agent-db-submit"]').click()

    await expect(page.getByText('Provisioned')).toBeVisible()
  })

  test('runs expired-deployment cleanup', async ({ page }) => {
    await page.goto('#/agent-dbs')

    await page.locator('[data-testid="agent-db-cleanup"]').click()

    await expect(page.getByText('Archived 1')).toBeVisible()
  })

  test('runs cloud provisioning preflight and dry-run execute', async ({
    page,
  }) => {
    await page.goto('#/agent-dbs')

    await page.getByText('Run preflight').click()
    await expect(page.getByText('Provision preflight passed')).toBeVisible()

    await page.getByText('Dry-run execute').click()
    await expect(page.getByText('Provision execute succeeded')).toBeVisible()
    await expect(page.locator('[data-testid="agent-db-provisioning"]'))
      .toContainText('workspace reachable')
  })

  test('runs cloud status, destroy dry-run, and reconcile actions', async ({
    page,
  }) => {
    await page.goto('#/agent-dbs')

    const statusRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith('/api/v1/agent-dbs/agentdb-demo/provision/status'))
    await page.getByText('Check status').click()
    await statusRequest
    await expect(page.getByText('Provision status succeeded')).toBeVisible()

    const destroyRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith(
        '/api/v1/agent-dbs/agentdb-demo/provision/destroy-dry-run',
      ))
    await page.getByText('Destroy dry-run').click()
    await destroyRequest
    await expect(page.getByText('Provision destroy dry-run succeeded'))
      .toBeVisible()

    const reconcileRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith('/api/v1/agent-dbs/reconcile'))
    await page.getByText('Reconcile abandoned').click()
    await reconcileRequest
    await expect(page.getByText('Reconciled 1 archived, 1 destroy dry-run'))
      .toBeVisible()
  })

  test('runs backup check and restore drill dry-run actions', async ({
    page,
  }) => {
    await page.goto('#/agent-dbs')

    const backupRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith('/api/v1/agent-dbs/agentdb-demo/backups/check'))
    await page.getByText('Check backups').click()
    await backupRequest
    await expect(page.getByText('Backup check verified')).toBeVisible()

    const restoreRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith(
        '/api/v1/agent-dbs/agentdb-demo/backups/restore-drill-dry-run',
      ))
    await page.getByText('Plan restore drill').click()
    await restoreRequest
    await expect(page.getByText('Restore drill dry-run planned')).toBeVisible()
  })

  test('creates a custom Lakebase instance size profile', async ({ page }) => {
    await page.goto('#/agent-dbs')

    await page.getByLabel('Profile ID').fill('lakebase_custom')
    await page.getByLabel('Name').fill('Lakebase custom')
    await page.locator('[data-testid="agent-db-profiles"] select')
      .first()
      .selectOption('databricks_lakebase')
    await page.locator('[data-testid="agent-db-profile-submit"]').click()

    await expect(page.getByText('Size profile saved')).toBeVisible()
  })

  test('creates and approves a promotion deploy request', async ({ page }) => {
    await page.goto('#/agent-dbs')

    await page.getByLabel('Promotion title').fill('Promote customer table')
    await page.getByLabel('Migration SQL')
      .fill('CREATE TABLE prod.customers(id bigint);')
    await page.getByLabel('Verification SQL')
      .fill('SELECT count(*) FROM prod.customers;')

    const createRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith('/api/v1/agent-dbs/agentdb-demo/deploy-requests'))
    await page.locator('[data-testid="agent-db-create-deploy-request"]').click()
    await createRequest
    await expect(page.getByText('Promotion draft recorded')).toBeVisible()

    const approveRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith(
        '/api/v1/agent-dbs/agentdb-demo/deploy-requests/dr-agentdb-demo/approve',
      ))
    await page.locator('[data-testid="agent-db-promotion"]')
      .getByRole('button', { name: 'Approve' })
      .click()
    await approveRequest
    await expect(page.getByText('Promotion approved')).toBeVisible()
  })
})
