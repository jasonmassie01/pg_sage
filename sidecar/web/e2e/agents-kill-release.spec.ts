// agents-kill-release.spec.ts — the Agents page kill switch, freezes
// banner, agent unfreeze and two-admin release in a browser (agentgov-07b;
// closes agentgov-07 CHECK-04 and the browser half of agentgov-06
// CHECK-05). The routes are mocked by e2e/mocks/agents.ts with one shared
// state; the server rules themselves are proven in Go.
//
// Concurrency: the two admins' browser contexts act in sequence on one
// shared mock state. True simultaneous release is a server property,
// tested in Go (internal/agentguard TestUnfreeze_ConcurrentSecondApprovers).
// Integration: the same scenario against the real full-surface stack
// (scripts/ci/full-surface-fixture.sh, CI's web-e2e-full job) is an owner
// action; it needs CI's PostgreSQL image with pgaudit.
import { test, expect, type Browser, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import {
  ADMIN1, ADMIN2, ALPHA_ID, AgentsMock, installAgentsMock, type MockUser,
} from './mocks/agents'

async function openAgents(page: Page, mock: AgentsMock, user: MockUser): Promise<void> {
  await mockAllAPIs(page)
  await installAgentsMock(page, mock, user)
  await page.goto('/#/agents')
  await expect(page.getByTestId('agents-table')).toBeVisible()
}

// asAdmin2 opens the Agents page in a second browser context as admin2.
async function asAdmin2(browser: Browser, mock: AgentsMock): Promise<Page> {
  const ctx = await browser.newContext()
  const page = await ctx.newPage()
  await openAgents(page, mock, ADMIN2)
  return page
}

async function killAll(page: Page, reason: string): Promise<void> {
  await page.getByTestId('agents-kill-open').click()
  await page.getByTestId('agents-kill-reason').fill(reason)
  await page.getByTestId('agents-kill-phrase').fill('KILL AGENTS')
  await page.getByTestId('agents-kill-confirm').click()
  await expect(page.getByTestId('agents-kill-report')).toBeVisible()
}

// release fills the freeze's reason, clicks Release and returns the
// response status and body.
async function release(page: Page, key: string, reason: string) {
  await page.getByTestId(`agents-freeze-reason-${key}`).fill(reason)
  const [resp] = await Promise.all([
    page.waitForResponse('**/api/v1/agents/kill/release'),
    page.getByTestId(`agents-freeze-release-${key}`).click(),
  ])
  return { status: resp.status(), body: await resp.json() }
}

async function unfreeze(page: Page, name: string, reason: string) {
  await page.getByTestId(`agent-open-${name}`).click()
  await expect(page.getByTestId('agent-detail-status')).toHaveText('frozen')
  await page.getByTestId('agent-unfreeze-reason').fill(reason)
  const [resp] = await Promise.all([
    page.waitForResponse(`**/api/v1/agents/*/unfreeze`),
    page.getByTestId('agent-unfreeze').click(),
  ])
  return { status: resp.status(), body: await resp.json() }
}

test.describe('Agents kill, unfreeze and second-admin release (mocked)', () => {
  test('admin1 kills all agents; admin2 completes the fleet release', async ({
    page, browser,
  }) => {
    // Two browser contexts and a reload: on a loaded machine the default
    // 30 s budget ran out once while opening the second context.
    test.slow()
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await expect(page.getByTestId('agents-freeze-banner')).toHaveCount(0)
    await killAll(page, 'runaway agent')

    expect(mock.calls('POST', '/api/v1/agents/kill').map(c => c.body))
      .toEqual([{ scope: 'all', reason: 'runaway agent' }])
    await expect(page.getByTestId('agents-kill-report')).toContainText(
      'Kill verified everywhere: 2 agents frozen, 2 tokens revoked, 0 approvals cancelled.')
    await expect(page.getByTestId('kill-db-orders')).toContainText('2 roles disabled')
    await expect(page.getByTestId('agents-kill-dialog')).toHaveCount(0)
    const fleet = page.getByTestId('agents-freeze-fleet')
    await expect(fleet).toContainText('Every agent is frozen (fleet)')
    await expect(fleet).toContainText('kill: runaway agent. Set by admin1@example.com')
    await expect(fleet).toContainText('A kill set it: releasing it needs two admins.')
    await expect(page.getByTestId('agent-status-alpha')).toHaveText('frozen')

    const first = await release(page, 'fleet', 'incident over')
    expect(first.status).toBe(202)
    expect(first.body).toEqual({ applied: false, pending: true, request_id: 43,
      requested_by: ADMIN1.email, quorum: 2 })
    await expect(page.getByTestId('agents-freeze-result-fleet')).toHaveText(
      'Pending second admin (request #43, started by admin1@example.com). ' +
      "Another admin, not an agent's sponsor, releases it too.")
    await expect(page.getByTestId('agents-freeze-pending-fleet'))
      .toContainText('Release #43 requested by admin1@example.com')

    const page2 = await asAdmin2(browser, mock)
    await expect(page2.getByTestId('agents-freeze-pending-fleet'))
      .toContainText('Release #43 requested by admin1@example.com')
    const second = await release(page2, 'fleet', 'confirmed over')
    expect(second.status).toBe(202)
    expect(second.body).toEqual({ applied: true, pending: false, request_id: 43,
      requested_by: ADMIN1.email, quorum: 2 })
    await expect(page2.getByTestId('agents-freeze-banner')).toHaveCount(0)

    // admin1's page reloads freezes on its next load, not by itself.
    await page.reload()
    await expect(page.getByTestId('agents-table')).toBeVisible()
    await expect(page.getByTestId('agents-freeze-banner')).toHaveCount(0)
    // Releasing the fleet flag does not unfreeze the agents themselves.
    await expect(page.getByTestId('agent-status-alpha')).toHaveText('frozen')
    expect(mock.calls('POST', '/api/v1/agents/kill/release').map(c => [c.user, c.body]))
      .toEqual([[ADMIN1.email, { scope: 'all', reason: 'incident over' }],
        [ADMIN2.email, { scope: 'all', reason: 'confirmed over' }]])
    await page2.context().close()
  })

  test('after a kill an agent needs a second admin who is not its sponsor', async ({
    page, browser,
  }) => {
    // Two browser contexts and a reload: on a loaded machine the default
    // 30 s budget ran out once while opening the second context.
    test.slow()
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await killAll(page, 'runaway agent')
    // beta's sponsor is admin2; alpha's is admin1.
    const started = await unfreeze(page, 'beta', 'beta is clean')
    expect(started.body).toMatchObject({ applied: false, pending: true, quorum: 2 })
    await expect(page.getByTestId('agent-unfreeze-result')).toContainText(
      `Waiting for a second admin (request #${started.body.request_id}, started by ` +
      'admin1@example.com)')

    const page2 = await asAdmin2(browser, mock)
    const refused = await unfreeze(page2, 'beta', 'agree')
    expect(refused.status).toBe(403)
    expect(refused.body.code).toBe('sponsor_cannot_approve')
    await expect(page2.getByTestId('agent-unfreeze-error')).toHaveText(
      "agentguard: the principal's sponsor cannot be the second approver" +
      '(sponsor_cannot_approve)')
    await expect(page2.getByTestId('agent-detail-status')).toHaveText('frozen')
    await expect(page2.getByTestId('agent-status-beta')).toHaveText('frozen')

    await page.getByTestId('agent-detail-close').click()
    await unfreeze(page, 'alpha', 'alpha is clean')
    await page2.getByTestId('agent-detail-close').click()
    const applied = await unfreeze(page2, 'alpha', 'agree')
    expect(applied.status).toBe(202)
    expect(applied.body).toMatchObject({ applied: true, pending: false, quorum: 2,
      requested_by: ADMIN1.email })
    await expect(page2.getByTestId('agent-detail-status')).toHaveText('active')
    await expect(page2.getByTestId('agent-status-alpha')).toHaveText('active')
    await expect(page2.getByTestId('agent-status-beta')).toHaveText('frozen')
    await page2.context().close()
  })

  test('the same admin releasing twice stays pending on one request', async ({ page }) => {
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await killAll(page, 'runaway agent')
    const first = await release(page, 'fleet', 'over')
    const again = await release(page, 'fleet', 'really over')
    expect(again.status).toBe(202)
    expect(again.body).toEqual({ applied: false, pending: true,
      request_id: first.body.request_id, requested_by: ADMIN1.email, quorum: 2 })
    await expect(page.getByTestId('agents-freeze-fleet')).toBeVisible()
    expect(mock.freezes).toHaveLength(1)
  })

  test('a stale release after the second admin applied it shows not_frozen', async ({
    page, browser,
  }) => {
    // Two browser contexts and a reload: on a loaded machine the default
    // 30 s budget ran out once while opening the second context.
    test.slow()
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await killAll(page, 'runaway agent')
    await release(page, 'fleet', 'over')
    const page2 = await asAdmin2(browser, mock)
    await release(page2, 'fleet', 'over')
    await page2.context().close()

    const stale = await release(page, 'fleet', 'over again')
    expect(stale.status).toBe(409)
    await expect(page.getByTestId('agents-freeze-error-fleet')).toHaveText(
      'agentguard: not frozen: no open fleet freeze ""(not_frozen)')
    await page.reload()
    await expect(page.getByTestId('agents-table')).toBeVisible()
    await expect(page.getByTestId('agents-freeze-banner')).toHaveCount(0)
  })

  test('kill needs the exact phrase and a reason; release needs a reason', async ({
    page,
  }) => {
    const mock = new AgentsMock()
    mock.seedFreeze('fleet')
    await openAgents(page, mock, ADMIN1)
    await page.getByTestId('agents-kill-open').click()
    const confirm = page.getByTestId('agents-kill-confirm')
    await page.getByTestId('agents-kill-reason').fill('runaway agent')
    await page.getByTestId('agents-kill-phrase').fill('kill agents')
    await expect(confirm).toBeDisabled()
    await page.getByTestId('agents-kill-phrase').fill('KILL AGENTS ')
    await expect(confirm).toBeDisabled()
    await page.getByTestId('agents-kill-phrase').fill('KILL AGENTS')
    await page.getByTestId('agents-kill-reason').fill('   ')
    await expect(confirm).toBeDisabled()
    await page.getByTestId('agents-kill-reason').fill('runaway agent')
    await expect(confirm).toBeEnabled()

    const releaseButton = page.getByTestId('agents-freeze-release-fleet')
    await expect(releaseButton).toBeDisabled()
    await page.getByTestId('agents-freeze-reason-fleet').fill('  ')
    await expect(releaseButton).toBeDisabled()
    expect(mock.calls('POST', '/api/v1/agents/kill')).toHaveLength(0)
    expect(mock.calls('POST', '/api/v1/agents/kill/release')).toHaveLength(0)
  })

  test('a 500 on kill keeps the dialog open with the error', async ({ page }) => {
    const mock = new AgentsMock()
    mock.failKill = true
    await openAgents(page, mock, ADMIN1)
    await page.getByTestId('agents-kill-open').click()
    await page.getByTestId('agents-kill-reason').fill('runaway agent')
    await page.getByTestId('agents-kill-phrase').fill('KILL AGENTS')
    await page.getByTestId('agents-kill-confirm').click()
    await expect(page.getByTestId('agents-kill-error')).toHaveText('internal error')
    await expect(page.getByTestId('agents-kill-dialog')).toBeVisible()
    await expect(page.getByTestId('agents-kill-confirm')).toBeEnabled()
    await expect(page.getByTestId('agents-kill-report')).toHaveCount(0)
    await expect(page.getByTestId('agents-freeze-banner')).toHaveCount(0)
    await expect(page.getByTestId('agent-status-alpha')).toHaveText('active')
  })

  test('no freezes shows no banner; a truncated list says so', async ({ page }) => {
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await expect(page.getByTestId('agents-freeze-banner')).toHaveCount(0)
    await expect(page.getByTestId('agents-freezes-truncated')).toHaveCount(0)

    mock.seedFreeze('database', 'orders', 'orders kill')
    mock.seedFreeze('fleet', '', 'fleet kill')
    mock.truncated = true
    await page.reload()
    const rows = page.getByTestId('agents-freeze-banner').locator('li')
    await expect(rows).toHaveCount(2)
    await expect(rows.nth(0)).toHaveAttribute('data-testid', 'agents-freeze-fleet')
    await expect(rows.nth(1)).toContainText('Agents on orders are frozen')
    await expect(page.getByTestId('agents-freezes-truncated'))
      .toHaveText('More freezes are open than shown (200 at most).')
  })

  test('a database kill needs a database; orders is sent as its id', async ({ page }) => {
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await page.getByTestId('agents-kill-open').click()
    await page.getByTestId('agents-kill-scope').selectOption('database')
    await page.getByTestId('agents-kill-reason').fill('orders leak')
    await page.getByTestId('agents-kill-phrase').fill('KILL AGENTS')
    const confirm = page.getByTestId('agents-kill-confirm')
    await expect(confirm).toBeDisabled()
    await expect(confirm).toHaveText('Kill agents on this database')
    await page.getByTestId('agents-kill-database').selectOption('orders')
    await expect(confirm).toBeEnabled()
    await confirm.click()

    expect(mock.calls('POST', '/api/v1/agents/kill').map(c => c.body))
      .toEqual([{ scope: 'database', id: 'orders', reason: 'orders leak' }])
    await expect(page.getByTestId('kill-db-orders')).toBeVisible()
    await expect(page.getByTestId('kill-db-primary')).toHaveCount(0)
    await expect(page.getByTestId('agents-freeze-database-orders'))
      .toContainText('Agents on orders are frozen')
    const first = await release(page, 'database-orders', 'fixed')
    expect(first.status).toBe(202)
    expect(mock.calls('POST', '/api/v1/agents/kill/release').map(c => c.body))
      .toEqual([{ scope: 'database', database: 'orders', reason: 'fixed' }])
  })

  test('an agent frozen from its drawer is unfrozen by one admin', async ({ page }) => {
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await page.getByTestId('agent-open-alpha').click()
    await expect(page.getByTestId('agent-detail-status')).toHaveText('active')
    await page.getByTestId('agent-freeze-reason').fill('investigating')
    await page.getByTestId('agent-freeze').click()
    await expect(page.getByTestId('agent-detail-status')).toHaveText('frozen')
    await expect(page.getByTestId('agent-detail-frozen-reason')).toHaveText('freeze: investigating')
    await expect(page.getByTestId('agent-status-alpha')).toHaveText('frozen')

    await page.getByTestId('agent-unfreeze-reason').fill('cleared')
    const [resp] = await Promise.all([
      page.waitForResponse(`**/api/v1/agents/${ALPHA_ID}/unfreeze`),
      page.getByTestId('agent-unfreeze').click(),
    ])
    expect(resp.status()).toBe(202)
    expect(await resp.json()).toMatchObject({ applied: true, pending: false, quorum: 1 })
    await expect(page.getByTestId('agent-detail-status')).toHaveText('active')
    await expect(page.getByTestId('agent-status-alpha')).toHaveText('active')
    await expect(page.getByTestId('agent-detail-frozen-reason')).toHaveCount(0)
    expect(mock.calls('POST', `/api/v1/agents/${ALPHA_ID}/freeze`).map(c => c.body))
      .toEqual([{ reason: 'investigating' }])
    expect(mock.calls('POST', `/api/v1/agents/${ALPHA_ID}/unfreeze`).map(c => c.body))
      .toEqual([{ reason: 'cleared' }])
  })

  test('a widening edit waits for a second admin', async ({ page }) => {
    const mock = new AgentsMock()
    await openAgents(page, mock, ADMIN1)
    await page.getByTestId('agent-open-alpha').click()
    await page.getByTestId('agent-edit-profile').selectOption('app-writer')
    await page.getByTestId('agent-edit-reason').fill('needs writes')
    const [resp] = await Promise.all([
      page.waitForResponse(r => r.request().method() === 'PATCH' &&
        r.url().endsWith(`/api/v1/agents/${ALPHA_ID}`)),
      page.getByTestId('agent-edit-submit').click(),
    ])
    expect(resp.status()).toBe(202)
    await expect(page.getByTestId('agent-edit-pending')).toContainText(
      'Awaiting a second admin: change #')
    await expect(page.getByTestId('agent-edit-pending'))
      .toContainText('requested by admin1@example.com')
    expect(mock.calls('PATCH', `/api/v1/agents/${ALPHA_ID}`).map(c => c.body))
      .toEqual([{ profile: 'app-writer', reason: 'needs writes' }])
    await expect(page.getByTestId('agent-status-alpha')).toHaveText('active')
  })
})
