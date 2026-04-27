import { expect, test } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('primary nav is contracted to autonomous DBA surfaces', async ({ page }) => {
  await page.goto('/#/users')

  await expect(page.getByTestId('nav-dashboard')).toContainText('Overview')
  await expect(page.getByTestId('nav-cases')).toContainText('Cases')
  await expect(page.getByTestId('nav-actions')).toContainText('Actions')
  await expect(page.getByTestId('nav-databases')).toContainText('Fleet')
  await expect(page.getByTestId('nav-settings')).toContainText('Settings')

  await expect(page.getByTestId('nav-query-hints')).toHaveCount(0)
  await expect(page.getByTestId('nav-schema-health')).toHaveCount(0)
  await expect(page.getByTestId('nav-incidents')).toHaveCount(0)
})
