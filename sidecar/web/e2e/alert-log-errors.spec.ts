// alert-log-errors.spec.ts (dashboard-16): alerting-09 CHECK-20. A broken
// Slack webhook leaves a red `error` row whose Error column carries the HTTP
// status. alerting-06c folded the Alert log into the Notifications Log tab:
// the old #/alerts bookmark redirects there, and the rows copy
// store.NotificationLogEntry (internal/store/notification_store.go) as
// notify's dispatcher writes them (status "error" with the redacted send
// error, internal/notify/dispatcher.go).
// The Alert log page's 120-character cut and summary tiles went with that
// page; the Log tab shows the whole error.
// No concurrency tests: the page is a read-only render of one response.

import { expect, test, type Page } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

const SLACK_404 = 'slack send failed permanently: slack returned status 404'
const PAGERDUTY_500 = 'pagerduty send failed after 3 attempts: pagerduty returned status 500'

function logRow(id: number, status: string, error = '') {
  return {
    id, channel_id: 3, event: 'alert', subject: 'Replica lag above 30 s', body: '',
    status, error, sent_at: '2026-11-10T09:00:00Z', database: 'primary',
  }
}

async function routeLog(page: Page, answer: () => { status?: number, json: unknown }) {
  await page.route('**/api/v1/notifications/log**', route => route.fulfill(answer()))
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
    await routeLog(page, () => ({ json: { log: [
      logRow(41, 'error', SLACK_404),
      logRow(40, 'sent'),
    ] } }))
    await page.goto('/#/alerts')
    await expect(page).toHaveURL(/#\/notifications\?tab=log/)
    const status = page.getByTestId('notify-status-41')
    await expect(status).toHaveText('error')
    const red = await cssColor(page, '--red')
    expect(red).not.toBe(await cssColor(page, '--green'))
    await expect(status).toHaveCSS('color', red)
    await expect(page.getByTestId('notify-error-41')).toHaveText(SLACK_404)
    await expect(page.getByTestId('notify-error-41')).toContainText('404')
    // The delivered row next to it is green with no error text.
    await expect(page.getByTestId('notify-status-40')).toHaveText('sent')
    await expect(page.getByTestId('notify-status-40'))
      .toHaveCSS('color', await cssColor(page, '--green'))
    await expect(page.getByTestId('notify-error-40')).toHaveText('-')
  })

test('a retried failure is red too and names the last status', async ({ page }) => {
  await routeLog(page, () => ({ json: { log: [logRow(7, 'error', PAGERDUTY_500)] } }))
  await page.goto('/#/alerts')
  const status = page.getByTestId('notify-status-7')
  await expect(status).toHaveText('error')
  await expect(status).toHaveCSS('color', await cssColor(page, '--red'))
  await expect(page.getByTestId('notify-error-7')).toHaveText(PAGERDUTY_500)
})

test('a long error is shown whole', async ({ page }) => {
  const long = SLACK_404 + ': ' + 'y'.repeat(200) + 'TAIL'
  await routeLog(page, () => ({ json: { log: [logRow(2, 'error', long)] } }))
  await page.goto('/#/alerts')
  await expect(page.getByTestId('notify-error-2')).toHaveText(long)
})

test('an empty log explains itself; a failed read is an error, not empty', async ({ page }) => {
  let answer: { status?: number, json: unknown } = { json: { log: [] } }
  await routeLog(page, () => answer)
  await page.goto('/#/alerts')
  await expect(page.getByText(/^No notifications sent yet\./)).toBeVisible()
  await expect(page.getByRole('alert')).toHaveCount(0)

  answer = { status: 503, json: { error: 'notification log unavailable' } }
  await page.reload()
  await expect(page.getByRole('alert'))
    .toContainText('Failed to load log: notification log unavailable (503)')
  await expect(page.getByText(/^No notifications sent yet\./)).toHaveCount(0)
})
