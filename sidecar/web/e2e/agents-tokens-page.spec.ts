// agents-tokens-page.spec.ts (dashboard-18): the MCP tokens page after
// agentgov-01 (reports/t0/agentgov-01.md, MANUAL "banner, columns"): the
// banner sending agent tokens to the Agents page, and the Principal and
// Owner columns. Payloads: e2e/mocks/tokens.ts.
// No concurrency tests: one admin drives one page.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { NOW, TOKEN_HEADERS, agentToken, operatorToken, routeTokens } from './mocks/tokens'

const cell = (page: Page, what: string, id: string) =>
  page.getByTestId(`mcp-token-${what}-${id}`)

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW)
  await mockAllAPIs(page)
})

test('agentgov-01: the banner points agent tokens at the Agents page', async ({ page }) => {
  await routeTokens(page, [agentToken('a01', 'laptop', 'active')])
  await page.goto('/#/mcp-tokens')
  const banner = page.getByTestId('mcp-tokens-agents-banner')
  await expect(banner).toHaveText(
    'Tokens for coding agents are issued per agent on the Agents page. Open Agents')
  await expect(page.getByTestId('mcp-tokens-explainer')).toContainText(
    'The tokens created here are operator tokens')

  await banner.getByTestId('mcp-tokens-agents-link').click()
  await expect(page).toHaveURL(/#\/agents$/)
  await expect(page.getByTestId('agents-page')).toBeVisible()
  await expect(page.getByTestId('mcp-tokens-page')).toHaveCount(0)
})

test('agentgov-01: the table names each token\'s principal or owner', async ({ page }) => {
  await routeTokens(page, [
    agentToken('a01', 'laptop', 'active'),
    operatorToken('o02', 'oncall-cursor', 5),
    operatorToken('o03', 'orphan', 42),
  ])
  await page.goto('/#/mcp-tokens')
  const headers = page.getByTestId('mcp-tokens-table').locator('thead th')
  await expect(headers).toHaveText(TOKEN_HEADERS)

  await expect(cell(page, 'principal', 'a01')).toHaveText('agp_laptop')
  await expect(cell(page, 'owner', 'a01')).toHaveText('-')
  await expect(cell(page, 'principal', 'o02')).toHaveText('-')
  await expect(cell(page, 'owner', 'o02')).toHaveText('oncall@x.test')
  // An owner the users list does not hold is named by id, never blank.
  await expect(cell(page, 'owner', 'o03')).toHaveText('user #42')
  await expect(cell(page, 'databases', 'o02')).toHaveText('All databases')
})

test('a retired or frozen agent\'s token says so instead of "active"', async ({ page }) => {
  await routeTokens(page, [
    agentToken('a01', 'laptop', 'active'),
    agentToken('a02', 'old-ci', 'retired'),
    agentToken('a03', 'paused', 'frozen'),
  ])
  await page.goto('/#/mcp-tokens')
  await expect(cell(page, 'status', 'a01')).toHaveText('active')
  await expect(cell(page, 'status', 'a02')).toHaveText('agent retired')
  await expect(cell(page, 'status', 'a03')).toHaveText('agent frozen')
  // A refused principal's token can still be revoked: revoking is the fix.
  await expect(cell(page, 'revoke', 'a02')).toBeVisible()
})

test('a token expiring exactly now is expired and offers no Revoke', async ({ page }) => {
  await routeTokens(page, [
    agentToken('a01', 'edge', 'active', { expires_at: NOW }),
    agentToken('a02', 'later', 'active',
      { expires_at: new Date(Date.parse(NOW) + 60000).toISOString() }),
  ])
  await page.goto('/#/mcp-tokens')
  await expect(cell(page, 'status', 'a01')).toHaveText('expired')
  await expect(cell(page, 'revoke', 'a01')).toHaveCount(0)
  await expect(cell(page, 'status', 'a02')).toHaveText('active')
  await expect(cell(page, 'revoke', 'a02')).toBeVisible()
})

test('no tokens shows the empty state with the banner', async ({ page }) => {
  await routeTokens(page, [])
  await page.goto('/#/mcp-tokens')
  await expect(page.getByTestId('mcp-tokens-empty')).toHaveText(
    'No MCP tokens yet. Create one above to connect a coding agent.')
  await expect(page.getByTestId('mcp-tokens-agents-banner')).toBeVisible()
  await expect(page.getByTestId('mcp-tokens-table')).toHaveCount(0)
})

test('a failed list names the server error', async ({ page }) => {
  await page.route(url => url.pathname === '/api/v1/mcp/tokens', route =>
    route.fulfill({ status: 503, json: { error: 'token store unavailable' } }))
  await page.goto('/#/mcp-tokens')
  await expect(page.getByTestId('mcp-tokens-error')).toHaveText(
    'Could not load MCP tokens: token store unavailable')
  await expect(page.getByTestId('mcp-tokens-empty')).toHaveCount(0)
})

test('the default create form posts an operator token for the chosen owner',
  async ({ page }) => {
    const posts = await routeTokens(page, [])
    await page.goto('/#/mcp-tokens')
    await expect(page.getByTestId('mcp-token-submit')).toBeDisabled()
    await page.getByTestId('mcp-token-name').fill('oncall-laptop')
    // Viewers cannot own a token: the select offers operators and admins.
    await expect(page.getByTestId('mcp-token-owner').locator('option')).toHaveText(
      ['Choose an owner', 'admin@x.test (admin)', 'oncall@x.test (operator)'])
    await page.getByTestId('mcp-token-owner').selectOption('5')
    await page.getByTestId('mcp-token-submit').click()
    await expect(page.getByTestId('mcp-token-secret')).toContainText('sage_mcp_SECRET')
    expect(posts).toEqual([{ name: 'oncall-laptop', kind: 'operator', scopes: ['read'],
      databases: ['*'], expires_in_days: 30, owner_user_id: 5 }])
    await expect(cell(page, 'owner', 'new-1')).toHaveText('oncall@x.test')
  })
