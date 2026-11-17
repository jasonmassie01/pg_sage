// evidence-packs.spec.ts (evid-02c): the database page's Evidence packs
// card: build a pack, preview what would leave (the model tier's sections
// and charts) and download the signed export. Payloads:
// e2e/mocks/workloadPacks.ts. No concurrency tests: one person drives one
// card.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'
import { packSHA, routePacks } from './mocks/workloadPacks'

const short = packSHA.slice(0, 12)

async function openDatabase(page: Page) {
  await page.goto('/#/database')
  await page.getByRole('combobox', { name: 'Database scope' }).selectOption('primary')
  await expect(page.getByTestId('evidence-packs')).toBeVisible()
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('builds a pack and lists it', async ({ page }) => {
  const seen = await routePacks(page, 'primary')
  await openDatabase(page)
  await expect(page.getByTestId('packs-empty')).toBeVisible()
  await page.getByTestId('pack-build').click()
  await expect(page.getByTestId(`pack-${short}`)).toContainText('2026-10-18 to 2026-11-15')
  expect(seen.filter(s => s.method === 'POST'))
    .toEqual([{ method: 'POST', url: '/api/v1/databases/primary/workload-packs' }])
})

test('previews what would leave: redacted sections and charts', async ({ page }) => {
  await routePacks(page, 'primary', true)
  await openDatabase(page)
  await page.getByTestId(`pack-preview-${short}`).click()
  await expect(page.getByTestId('model-tier-size')).toContainText('2 charts')
  await page.getByTestId('model-section-texts.json').locator('summary').click()
  await expect(page.getByTestId('model-section-texts.json')).toContainText("token = '?'")
  await expect(page.getByRole('img', { name: /hour-of-week exec time/ })).toBeVisible()
  await expect(page.getByRole('img', { name: /weekly exec time/ })).toBeVisible()
})

test('downloads the signed export', async ({ page }) => {
  await routePacks(page, 'primary', true)
  await openDatabase(page)
  const download = page.waitForEvent('download')
  await page.getByTestId(`pack-export-${short}`).click()
  expect((await download).suggestedFilename())
    .toBe(`pg_sage-workload-primary-${short}.tar.gz`)
})
