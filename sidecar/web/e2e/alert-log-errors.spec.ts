// alert-log-errors.spec.ts (dashboard-16): alerting-09 CHECK-20. A broken
// Slack webhook leaves a red `error` row in the Alert log whose Error column
// carries the HTTP status (internal/alerting/httpsend.go statusError, wrapped
// by sendWithRetry). Rows copy alertLogHandler's keys
// (internal/api/handlers_new.go).
// No concurrency tests: the page is a read-only render of one response.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

// A 404 is permanent (retryable is false for 4xx but 429).
const SLACK_404 = 'slack send failed permanently: slack returned status 404'
// A 5xx is retried three times before the row is written.
const PAGERDUTY_500 = 'pagerduty send failed after 3 attempts: pagerduty returned status 500'

function alertRow(id: number, status: string, channel: string, error_message = '') {
  return {
    id, sent_at: '2026-11-10T09:00:00Z', severity: 'critical', channel, status,
    error_message, category: 'replication_lag', title: 'Replica lag above 30 s',
    database_name: 'primary',
  }
}

async function routeAlertLog(page: Page, answer: () => { status?: number, json: unknown }) {
  await page.route('**/api/v1/alert-log**', route => route.fulfill(answer()))
}

// cssColor resolves a theme variable to the rgb() string getComputedStyle reports.
function cssColor(page: Page, variable: string) {
  return page.evaluate(v => {
    const probe = document.createElement('span')
    probe.style.color = `var(${v})`
    document.body.appendChild(probe)
    const color = getComputedStyle(probe).color
    probe.remove()
    return color
  }, variable)
}

test.beforeEach(async ({ page }) => {
  await mockAllAPIs(page)
})

test('alerting-09 CHECK-20: a broken Slack webhook shows a red error row with the status',
  async ({ page }) => {
    await routeAlertLog(page, () => ({ json: { alerts: [
      alertRow(41, 'error', 'slack', SLACK_404),
      alertRow(40, 'sent', 'webhook'),
    ] } }))
    await page.goto('/#/alerts')
    const status = page.getByTestId('alert-status-41')
    await expect(status).toHaveText('error')
    const red = await cssColor(page, '--red')
    expect(red).not.toBe(await cssColor(page, '--green'))
    await expect(status).toHaveCSS('color', red)
    await expect(page.getByTestId('alert-error-41')).toHaveText(SLACK_404)
    await expect(page.getByTestId('alert-error-41')).toContainText('404')
    // The delivered row next to it is green with no error text.
    await expect(page.getByTestId('alert-status-40')).toHaveText('sent')
    await expect(page.getByTestId('alert-status-40'))
      .toHaveCSS('color', await cssColor(page, '--green'))
    await expect(page.getByTestId('alert-error-40')).toHaveText('-')
    await expect(page.getByTestId('alert-count-failed')).toContainText('1')
    await expect(page.getByTestId('alert-count-delivered')).toContainText('1')
  })

test('a failed resolve is red too and names the retried status', async ({ page }) => {
  await routeAlertLog(page, () => ({ json: { alerts: [
    alertRow(7, 'resolve_error', 'pagerduty', PAGERDUTY_500),
  ] } }))
  await page.goto('/#/alerts')
  const status = page.getByTestId('alert-status-7')
  await expect(status).toHaveText('resolve failed')
  await expect(status).toHaveCSS('color', await cssColor(page, '--red'))
  await expect(page.getByTestId('alert-error-7')).toHaveText(PAGERDUTY_500)
  await expect(page.getByTestId('alert-count-failed')).toContainText('1')
})

test('an error longer than 120 characters is cut with more; 120 is shown whole',
  async ({ page }) => {
    const exact = 'x'.repeat(120)
    const long = SLACK_404 + ': ' + 'y'.repeat(120 - SLACK_404.length - 2) + 'TAIL'
    await routeAlertLog(page, () => ({ json: { alerts: [
      alertRow(1, 'error', 'slack', exact), alertRow(2, 'error', 'slack', long),
    ] } }))
    await page.goto('/#/alerts')
    await expect(page.getByTestId('alert-error-1')).toHaveText(exact)
    await expect(page.getByTestId('alert-error-1').getByRole('button')).toHaveCount(0)
    const cut = page.getByTestId('alert-error-2')
    await expect(cut).toHaveText(long.slice(0, 120) + '…more')
    await cut.getByRole('button', { name: 'more' }).click()
    await expect(cut).toHaveText(long + 'less')
  })

test('an empty log explains itself; a failed read is an error, not empty', async ({ page }) => {
  let answer: { status?: number, json: unknown } = { json: { alerts: [] } }
  await routeAlertLog(page, () => answer)
  await page.goto('/#/alerts')
  await expect(page.getByText(/^No alerts sent yet\./)).toBeVisible()
  await expect(page.getByTestId('alert-log-summary')).toHaveCount(0)

  answer = { status: 503, json: { error: 'alert log unavailable' } }
  await page.reload()
  await expect(page.getByRole('alert')).toContainText('alert log unavailable (503)')
  await expect(page.getByText(/^No alerts sent yet\./)).toHaveCount(0)
})
