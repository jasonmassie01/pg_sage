import { test, expect } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { makeAgentDBDeployments, registerAgentDBAPIs } from './agentdb-fixtures'

test.describe('Agent DBs page', () => {
  test.beforeEach(async ({ page }) => {
    await mockAllAPIs(page)
    page.on('dialog', dialog => dialog.accept())
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
    await expect(page.locator('[data-testid="agent-db-provisioning"]'))
      .toContainText('Cloud provisioning')
    await expect(page.locator('[data-testid="agent-db-provisioning"]'))
      .toContainText('dry run: databricks database branches create')
    await expect(page.locator('[data-testid="agent-db-backup-assurance"]'))
      .toContainText('restore_verified')
    await page.locator('#agent-db-tab-activity').click()
    await expect(page.locator('[data-testid="agent-db-requests"]')).toContainText(
      'req-agentdb-demo',
    )
    await page.locator('#agent-db-tab-profiles').click()
    await expect(page.locator('[data-testid="agent-db-providers"]')).toContainText(
      'Databricks Lakebase',
    )
    await expect(page.locator('[data-testid="agent-db-profiles"]')).toContainText(
      'Lakebase instance S',
    )
  })

  test('provisions through the UI request path', async ({ page }) => {
    await page.goto('#/agent-dbs')

    await page.locator('#agent-db-tab-provision').click()
    await page.locator('[data-testid="agent-db-submit"]').click()

    await expect(page.locator('[data-testid="agent-db-message"]'))
      .toContainText(/Provisioned tenant_agent_agent_runner_/)
    await expect(page.locator('[data-testid="agent-db-view-deployment"]'))
      .toBeVisible()
    await expect(page.getByRole('tab', { name: 'Deployments' }))
      .toHaveAttribute('aria-selected', 'true')
    await expect(page.locator('[data-testid="agent-db-detail"]'))
      .toContainText(/tenant_agent_agent_runner_/)
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

  test('runs live cloud execute and live destroy controls', async ({ page }) => {
    await page.goto('#/agent-dbs')

    const executeRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith('/api/v1/agent-dbs/agentdb-demo/provision/execute') &&
      request.postDataJSON().mode === 'live')
    await page.getByText('Live execute').click()
    await executeRequest
    await expect(page.getByText('Provision live execute succeeded')).toBeVisible()

    const destroyRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith('/api/v1/agent-dbs/agentdb-demo/provision/destroy-live'))
    await page.getByText('Destroy live').click()
    await destroyRequest
    await expect(page.getByText('Provision destroy live succeeded')).toBeVisible()
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

    await page.locator('#agent-db-tab-profiles').click()
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

  test('approves and provisions Terraform templates', async ({ page }) => {
    await page.goto('#/agent-dbs')
    await page.locator('#agent-db-tab-terraform').click()

    const approveRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith(
        '/api/v1/agent-dbs/terraform-templates/tf-agentdb-draft/approve',
      ))
    await page.locator('[data-testid="agent-db-terraform-approve"]').click()
    await approveRequest
    await expect(page.getByText('Terraform template approved')).toBeVisible()

    const provisionRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith(
        '/api/v1/agent-dbs/terraform-templates/tf-agentdb-approved/provision',
      ))
    await page.locator('[data-testid="agent-db-terraform-provision"]').click()
    await provisionRequest
    await expect(page.getByText(
      'Terraform template provisioned as tf-agentdb-approved',
    ))
      .toBeVisible()
  })

  test('approves and provisions blueprints', async ({ page }) => {
    await page.goto('#/agent-dbs')
    await page.locator('#agent-db-tab-blueprints').click()

    const approveRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith(
        '/api/v1/agent-dbs/blueprints/bp-agentdb-generated/approve',
      ))
    await page.locator('[data-testid="agent-db-blueprint-approve"]').click()
    await approveRequest
    await expect(page.getByText('Blueprint approved')).toBeVisible()

    const provisionRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith(
        '/api/v1/agent-dbs/blueprints/bp-agentdb-approved/provision',
      ))
    await page.locator('[data-testid="agent-db-blueprint-provision"]').click()
    await provisionRequest
    await expect(page.getByText(
      'Blueprint provisioned as bp-agentdb-approved',
    ))
      .toBeVisible()
  })

  test('provisions approved agent API requests', async ({ page }) => {
    await page.goto('#/agent-dbs')
    await page.locator('#agent-db-tab-activity').click()

    const provisionRequest = page.waitForRequest(request =>
      request.method() === 'POST' &&
      request.url().endsWith('/api/v1/agent-dbs/requests/req-agentdb-demo/provision'))
    await page.locator('[data-testid="agent-db-requests"]')
      .getByRole('button', { name: 'Provision' })
      .click()
    await provisionRequest
    await expect(page.getByText(
      /Approved agent request provisioned as req_agentdb_demo_/,
    ))
      .toBeVisible()
  })

  test('keeps selected deployment detail visible with a long list', async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1264, height: 800 })
    await registerAgentDBAPIs(page, {
      deployments: makeAgentDBDeployments(36),
    })
    await page.goto('#/agent-dbs')

    const rows = page.locator('[data-testid="agent-db-row"]')
    await expect(rows).toHaveCount(36)
    await rows.nth(25).getByRole('button').first().click()

    const detail = page.locator('[data-testid="agent-db-detail"]')
    await expect(detail).toContainText('agentdb-extra-25')
    await expect(detail).toBeInViewport({ ratio: 0.25 })
  })

  test('moves selection after deleting the selected deployment', async ({
    page,
  }) => {
    await registerAgentDBAPIs(page, {
      deployments: makeAgentDBDeployments(3),
    })
    await page.goto('#/agent-dbs')

    await page.locator('[data-testid="agent-db-row"]').nth(1)
      .getByRole('button')
      .first()
      .click()
    await expect(page.locator('[data-testid="agent-db-detail"]'))
      .toContainText('agentdb-extra-01')

    await page.locator('[data-testid="agent-db-row"]').nth(1)
      .getByRole('button', { name: 'Delete' })
      .click()

    await expect(page.locator('[data-testid="agent-db-detail"]'))
      .toContainText('agentdb-demo')
    await expect(page.locator('[data-testid="agent-db-row"]')).toHaveCount(2)
  })
})
