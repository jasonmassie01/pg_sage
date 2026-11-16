// setup-wizard.spec.ts (ux-04b): the four-step setup wizard on the home
// page, in the real bundle with real clicks, from zero databases to the
// first look. Payloads and the moving server state: e2e/mocks/setup.ts.
// Concurrency: a second admin's config write first makes the autonomy PUT
// a 409 (the config generation check), asserted below.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { report, routeFirstLook } from './mocks/firstlook'
import { DB, DB_CONFIG_URL, newSetupState, routeSetup, type SetupState } from './mocks/setup'

const URL_RDS = 'postgres://sage_agent:Sup3r%21@orders.c9.us-east-1.rds.amazonaws.com:5432/' +
  'orders?sslmode=verify-full'

let state: SetupState

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeFirstLook(page, () => ({ json: { databases: state.connected
    ? [{ database: DB, report: report(DB) }] : [] } }))
  state = newSetupState()
  await routeSetup(page, state)
})

const wizard = (page: Page) => page.getByTestId('setup-wizard')
const stepNav = (page: Page, key: string) => page.getByTestId(`setup-step-${key}`)

async function connect(page: Page) {
  await page.getByTestId('setup-url').fill(URL_RDS)
  await page.getByTestId('setup-url-parse').click()
  await expect(page.getByTestId('setup-provider')).toHaveValue('rds')
  await expect(page.getByTestId('setup-role-sql')).toContainText('rds_superuser')
  await expect(page.getByTestId('db-password')).toHaveAttribute('type', 'password')
  await page.getByTestId('db-save-button').click()
  await expect(stepNav(page, 'connect')).toHaveAttribute('data-status', 'done')
  await expect(page.getByTestId('doctor-check-extensions')).toHaveAttribute('data-status',
    'warn')
  await page.getByTestId('setup-next').click()
}

async function model(page: Page) {
  await page.getByTestId('setup-llm-endpoint').fill('https://llm.example/v1')
  await page.getByTestId('setup-llm-model').fill('model-a')
  await page.getByTestId('setup-llm-key').fill('sk-test-key')
  await page.getByTestId('setup-llm-test').click()
}

async function channel(page: Page) {
  await page.getByTestId('add-channel-name').fill('ops')
  await page.getByTestId('add-channel-webhook-url')
    .fill('https://hooks.slack.com/services/T/B/X')
  await page.getByTestId('add-channel-submit').click()
  await page.getByTestId('setup-channel-test').click()
}

test('zero databases: four steps, then the first look, and the checklist has Model done',
  async ({ page }) => {
    await page.goto('/#/')
    await expect(wizard(page)).toBeVisible()
    await expect(page.getByTestId('inbox-page')).toHaveCount(0)
    await connect(page)
    await model(page)
    await expect(page.getByTestId('setup-llm-result')).toContainText('answered in 310 ms')
    await expect(stepNav(page, 'model')).toHaveAttribute('data-status', 'done')
    await channel(page)
    await expect(stepNav(page, 'channel')).toHaveAttribute('data-status', 'done')
    await page.getByRole('radio', { name: /Ask me first/ }).check()
    await page.getByTestId('setup-autonomy-apply').click()
    await expect(page.getByTestId('setup-finished')).toBeVisible()
    await expect(page.getByRole('region', { name: 'First look' })).toBeVisible()
    const checklist = page.getByRole('list', { name: 'Setup checklist' })
    await expect(checklist.locator('li').filter({ hasText: 'Model' }))
      .toHaveAttribute('data-done', 'true')
    const put = state.writes.find(w => w.path === DB_CONFIG_URL)
    expect(put?.body).toEqual({ 'trust.level': 'advisory', execution_mode: 'approval',
      expected_generation: 2 })
    expect(Object.keys(state.setup).sort()).toEqual(['autonomy', 'channel', 'model'])
    // The pasted password never reaches the page text.
    expect(await page.locator('body').innerText()).not.toContain('Sup3r!')
  })

test('a bad URL shows a field error and saves nothing', async ({ page }) => {
  await page.goto('/#/')
  await page.getByTestId('setup-url').fill('postgres://u:p@h/orders?sslmode=bogus')
  await page.getByTestId('setup-url-parse').click()
  await expect(page.getByTestId('setup-url-error')).toContainText('sslmode "bogus"')
  await expect(page.getByTestId('db-form')).toHaveCount(0)
  expect(state.writes).toEqual([])
})

test('a refused model key shows its own message and Skip says what it costs',
  async ({ page }) => {
    state.llmClass = 'auth'
    await page.goto('/#/')
    await connect(page)
    await model(page)
    await expect(page.getByTestId('setup-llm-result')).toContainText('refused the key')
    await expect(stepNav(page, 'model')).toHaveAttribute('data-status', 'todo')
    await page.getByTestId('setup-step-body').getByRole('button', { name: /skip/i }).click()
    await expect(page.getByTestId('setup-skip-note-model'))
      .toHaveText('Skipped: LLM features stay off until a model is set')
    expect(state.setup.model).toBeUndefined()
  })

test('two admins at step 4: the stale write is refused with 409 and nothing is recorded',
  async ({ page }) => {
    state.configConflict = true
    await page.goto('/#/')
    await connect(page)
    await page.getByTestId('setup-step-body').getByRole('button', { name: /skip/i }).click()
    await page.getByTestId('setup-step-body').getByRole('button', { name: /skip/i }).click()
    await page.getByRole('radio', { name: /Watch only/ }).check()
    await page.getByTestId('setup-autonomy-apply').click()
    await expect(page.getByTestId('setup-autonomy-error'))
      .toContainText('Another admin changed the configuration first')
    expect(state.setup.autonomy).toBeUndefined()
    await expect(stepNav(page, 'autonomy')).toHaveAttribute('data-status', 'todo')
  })
