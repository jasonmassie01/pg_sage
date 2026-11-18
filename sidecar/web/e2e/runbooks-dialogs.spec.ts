// runbooks-dialogs.spec.ts (dashboard-18): the Runbooks page's revise,
// sign-diff and retire dialogs (playbooks-10, reports/t2/playbooks-10.md:124
// MANUAL "the Runbooks page in a browser"). Payloads: e2e/mocks/runbooks.ts.
// No concurrency tests: one user drives one runbook; the server's
// base_version check is covered by the Go tests.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs, mockAuthMe } from './fixtures'
import { routeFleet } from './mocks/cases'
import {
  HASH_V2, V1_DEFINITION, routeRunbooks, runbook, version, type WriteAnswer,
} from './mocks/runbooks'

type State = { rb: ReturnType<typeof runbook> }

async function openRunbook(page: Page, state: State,
  refuse: (path: string) => WriteAnswer = () => null) {
  const writes = await routeRunbooks(page, state, refuse)
  await page.goto('/#/advanced/runbooks')
  await expect(page.getByText('Select a database to manage its runbooks.')).toBeVisible()
  await page.getByRole('combobox', { name: 'Database scope' }).selectOption('orders')
  await page.getByTestId('runbook-row-rb-1').click()
  await expect(page.getByTestId('runbook-detail')).toBeVisible()
  return writes
}

const signedV1 = (name = 'Idle holder') => ({ rb: runbook(name, [version(1, name, true)]) })

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
  await routeFleet(page)
})

test('playbooks-10: revising stages a draft while the signed version keeps running',
  async ({ page }) => {
    const state = signedV1()
    const writes = await openRunbook(page, state)
    await expect(page.getByTestId('runbook-running-status')).toHaveText('v1 signed and running')
    await page.getByTestId('runbook-revise').click()
    const form = page.getByTestId('runbook-revise-form')
    await expect(form).toContainText('Revise version 1')
    await expect(form.getByTestId('runbook-revise-note'))
      .toHaveText('v1 keeps running; v2 is a draft until an admin signs it')
    // The form starts from the latest definition, as JSON.
    expect(JSON.parse(await form.getByTestId('runbook-revise-json').inputValue()))
      .toEqual(V1_DEFINITION)
    await form.getByTestId('runbook-revise-submit').click()
    await expect(page.getByTestId('runbook-running-status'))
      .toHaveText('v1 signed and running; v2 draft awaiting signature')
    await expect(form).toHaveCount(0)
    expect(writes).toEqual([{ path: '/rb-1/versions',
      body: { base_version: 1, definition: V1_DEFINITION } }])
  })

test('a 500 on revise keeps the form open with the server error', async ({ page }) => {
  const state = signedV1()
  const writes = await openRunbook(page, state,
    () => ({ status: 500, json: { error: 'runbook store unavailable' } }))
  await page.getByTestId('runbook-revise').click()
  await page.getByTestId('runbook-revise-submit').click()
  await expect(page.getByTestId('runbook-problems')).toContainText('runbook store unavailable')
  await expect(page.getByTestId('runbook-revise-form')).toBeVisible()
  await expect(page.getByTestId('runbook-running-status')).toHaveText('v1 signed and running')
  expect(writes).toHaveLength(1)
})

test('a revision that is not JSON is refused before anything is sent', async ({ page }) => {
  const writes = await openRunbook(page, signedV1())
  await page.getByTestId('runbook-revise').click()
  await page.getByTestId('runbook-revise-json').fill('{ "nodes": [')
  await page.getByTestId('runbook-revise-submit').click()
  await expect(page.getByTestId('runbook-problems')).toContainText('Revision is not valid JSON')
  await expect(page.getByTestId('runbook-revise-form')).toBeVisible()
  expect(writes).toEqual([])
})

test('playbooks-10: signing shows the structural diff from the newest signed version',
  async ({ page }) => {
    const name = 'Idle holder'
    const state = { rb: runbook(name, [version(1, name, true), version(2, name, false)],
      'draft') }
    const writes = await openRunbook(page, state)
    await page.getByTestId('runbook-sign').click()
    const dialog = page.getByRole('dialog', { name: 'Sign runbook' })
    await expect(dialog).toContainText('Sign version 2')
    await expect(dialog).toContainText('Changes against v1, the newest signed version:')
    await expect(dialog.getByTestId('runbook-sign-diff').locator('li'))
      .toHaveText(['added node is_idle', 'changed node read_chains: next'])
    await expect(dialog).toContainText(`Content hash: ${HASH_V2}`)
    await dialog.getByTestId('runbook-sign-confirm').click()
    await expect(page.getByTestId('runbook-running-status')).toHaveText('v2 signed and running')
    expect(writes).toEqual([{ path: '/rb-1/sign', body: { version: 2, content_hash: HASH_V2 } }])
  })

test('ux-12: a 500 on sign keeps the dialog open with the server error inside it',
  async ({ page }) => {
    const name = 'Idle holder'
    const state = { rb: runbook(name, [version(1, name, true), version(2, name, false)],
      'draft') }
    const writes = await openRunbook(page, state,
      path => (path.endsWith('/sign')
        ? { status: 500, json: { error: 'runbook store unavailable' } } : null))
    await page.getByTestId('runbook-sign').click()
    const dialog = page.getByRole('dialog', { name: 'Sign runbook' })
    await dialog.getByTestId('runbook-sign-confirm').click()
    await expect(dialog.getByTestId('runbook-dialog-problems'))
      .toContainText('runbook store unavailable')
    await expect(dialog.getByTestId('runbook-dialog-problems')).toBeVisible()
    await expect(dialog.getByTestId('runbook-sign-confirm')).toBeEnabled()
    await expect(page.getByTestId('runbook-running-status'))
      .toHaveText('v1 signed and running; v2 draft awaiting signature')
    expect(writes).toHaveLength(1)
  })

test('a first signature says no version was signed before', async ({ page }) => {
  const state = { rb: runbook('Fresh', [version(1, 'Fresh', false)], 'draft') }
  await openRunbook(page, state)
  await expect(page.getByTestId('runbook-running-status'))
    .toHaveText('v1 draft: runbook not running')
  await page.getByTestId('runbook-sign').click()
  const dialog = page.getByTestId('runbook-sign-dialog')
  await expect(dialog).toContainText(
    'First signature: no version of this runbook was signed before.')
  await expect(dialog.getByTestId('runbook-sign-diff')).toHaveCount(0)
})

test('playbooks-10: retiring needs the exact name, shown as text', async ({ page }) => {
  const name = 'Idle <img src=x onerror=alert(1)> holder'
  const state = signedV1(name)
  const writes = await openRunbook(page, state)
  await page.getByTestId('runbook-retire').click()
  const dialog = page.getByRole('dialog', { name: 'Retire runbook' })
  await expect(dialog).toContainText(
    `Retiring stops every version of ${name} for good; it cannot be undone.`)
  await expect(page.locator('img[src="x"]')).toHaveCount(0)
  const confirm = dialog.getByTestId('runbook-retire-confirm')
  await expect(confirm).toBeDisabled()
  await dialog.getByTestId('runbook-retire-name').fill(name.toUpperCase())
  await expect(confirm).toBeDisabled()
  await dialog.getByTestId('runbook-retire-name').fill(name)
  await expect(confirm).toBeEnabled()
  await confirm.click()
  await expect(page.getByTestId('runbook-running-status'))
    .toHaveText('retired: runbook not running')
  await expect(page.getByTestId('runbook-revise')).toHaveCount(0)
  await expect(page.getByTestId('runbook-retire')).toHaveCount(0)
  expect(writes).toEqual([{ path: '/rb-1/retire', body: null }])
})

test('an operator may revise but not sign or retire; a viewer only reads',
  async ({ page }) => {
    let role = 'operator'
    await page.route('**/api/v1/auth/me', route =>
      route.fulfill({ json: { ...mockAuthMe, role } }))
    const name = 'Idle holder'
    await openRunbook(page, { rb: runbook(name, [version(1, name, true),
      version(2, name, false)], 'draft') })
    await expect(page.getByTestId('runbook-revise')).toBeVisible()
    await expect(page.getByTestId('runbook-sign')).toHaveCount(0)
    await expect(page.getByTestId('runbook-retire')).toHaveCount(0)

    role = 'viewer'
    await page.reload()
    await page.getByRole('combobox', { name: 'Database scope' }).selectOption('orders')
    await page.getByTestId('runbook-row-rb-1').click()
    await expect(page.getByTestId('runbook-detail')).toBeVisible()
    await expect(page.getByTestId('runbook-revise')).toHaveCount(0)
    await expect(page.getByTestId('runbook-create')).toHaveCount(0)
  })
