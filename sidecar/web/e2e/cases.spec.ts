import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('Cases page loads and old findings route aliases to cases', async ({ page }) => {
  await page.goto('/#/cases')
  await expect(page.locator('header h1')).toContainText('Cases')
  await expect(page.getByTestId('cases-page')).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Stats are stale', exact: true })).toBeVisible()
  await expect(page.getByText(/Policy: execute/)).not.toBeVisible()
  await page.getByTestId('case-technical-details').first().locator('summary').click()
  await expect(page.getByText(/Policy: execute/)).toBeVisible()
  await expect(page.getByText('dedicated connection')).toBeVisible()
  await expect(page.getByText(/Lifecycle: blocked/)).toBeVisible()
  await expect(page.getByText(/Lifecycle: executed/)).toBeVisible()
  await expect(page.getByText(/Verification: verified/)).toBeVisible()
  await expect(page.getByLabel('Action timeline')
    .getByText('action is in cooldown')).toBeVisible()

  await page.goto('/#/findings')
  await expect(page.locator('header h1')).toContainText('Cases')
  await expect(page.getByTestId('cases-page')).toBeVisible()
})
