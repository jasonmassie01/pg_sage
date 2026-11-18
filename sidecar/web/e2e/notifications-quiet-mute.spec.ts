// notifications-quiet-mute.spec.ts — ux-09b: a channel's own quiet hours
// on the channel form, the mute dialog (from the page and from a link),
// unmute, and the weekly line. Every /api route is mocked.

import { test, expect, type Page, type Route } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

type Mute = { database: string; class: string; until: string; actor: string; reason: string }

async function mockQuietMute(page: Page, weekly: Record<string, unknown>) {
  const sent: { method: string; url: string; body: unknown }[] = []
  const mutes: Mute[] = []
  const channel = {
    id: 5, name: 'ops-slack', type: 'slack', enabled: true,
    config: { webhook_url: 'https://hooks.slack.com/services/T/B/****' },
    last_status: '', last_error: '', last_sent_at: null,
  } as Record<string, unknown>
  await page.route('**/api/v1/notifications/channels**', async (route: Route) => {
    const req = route.request()
    if (req.method() === 'GET') return route.fulfill({ json: { channels: [channel] } })
    const body = req.postDataJSON()
    sent.push({ method: req.method(), url: req.url(), body })
    if (req.url().endsWith('/quiet-hours')) {
      if (body.start === body.end && body.start) {
        return route.fulfill({ status: 400, json: {
          error: `quiet hours start "${body.start}" and end "${body.end}" are equal` } })
      }
      channel.quiet_hours_start = body.start
      channel.quiet_hours_end = body.end
      channel.quiet_hours_timezone = body.timezone
    }
    return route.fulfill({ json: { status: 'updated' } })
  })
  await page.route('**/api/v1/notifications/mutes**', async (route: Route) => {
    const req = route.request()
    if (req.method() === 'GET') return route.fulfill({ json: { mutes } })
    if (req.method() === 'DELETE') {
      sent.push({ method: 'DELETE', url: req.url(), body: null })
      const db = new URL(req.url()).searchParams.get('database')
      mutes.splice(0, mutes.length, ...mutes.filter(m => m.database !== db))
      return route.fulfill({ json: { status: 'unmuted' } })
    }
    const body = req.postDataJSON()
    sent.push({ method: 'POST', url: req.url(), body })
    const m = { database: body.database, class: body.class, until: body.until,
      actor: 'test@test.com', reason: body.reason }
    mutes.push(m)
    return route.fulfill({ status: 201, json: m })
  })
  await page.route('**/api/v1/notifications/weekly-line', route =>
    route.fulfill({ json: weekly }))
  return sent
}

test('sets, refuses and clears a channel\'s quiet hours', async ({ page }) => {
  await mockAllAPIs(page)
  const sent = await mockQuietMute(page, { messages: 0, led_to_action: 0,
    text: 'No messages this week' })
  await page.goto('/#/notifications')
  await expect(page.getByTestId('weekly-line')).toHaveText('This week: No messages this week')

  const row = page.getByTestId('channel-row').filter({ hasText: 'ops-slack' })
  await row.getByTestId('channel-edit-button').click()
  const dialog = page.getByRole('dialog', { name: /edit channel/i })
  await dialog.getByTestId('quiet-start').fill('22:00')
  await dialog.getByTestId('quiet-end').fill('22:00')
  await dialog.getByTestId('quiet-timezone').fill('America/Chicago')
  await dialog.getByTestId('quiet-save').click()
  await expect(dialog.getByText(/are equal/)).toBeVisible()

  await dialog.getByTestId('quiet-end').fill('07:00')
  await dialog.getByTestId('quiet-save').click()
  await expect(dialog.getByText('Quiet hours saved')).toBeVisible()
  const ok = sent.filter(s => s.url.endsWith('/channels/5/quiet-hours'))
  expect(ok[ok.length - 1]).toEqual({ method: 'PUT', url: expect.any(String),
    body: { start: '22:00', end: '07:00', timezone: 'America/Chicago' } })
  await expect(row.getByTestId('channel-quiet-hours'))
    .toHaveText('quiet 22:00-07:00 America/Chicago')

  await dialog.getByTestId('quiet-clear').click()
  await expect(dialog.getByText('Quiet hours cleared')).toBeVisible()
  expect(sent[sent.length - 1].body).toEqual({ start: '', end: '', timezone: '' })
})

test('mutes a database, lists it and unmutes it', async ({ page }) => {
  await mockAllAPIs(page)
  const sent = await mockQuietMute(page, { messages: 12, led_to_action: 5,
    text: '12 messages, 5 led to an action' })
  await page.goto('/#/notifications')
  await expect(page.getByTestId('weekly-line'))
    .toHaveText('This week: 12 messages, 5 led to an action')
  await expect(page.getByTestId('mutes-empty')).toBeVisible()

  await page.getByTestId('mute-database-button').click()
  const dialog = page.getByRole('dialog', { name: 'Mute database' })
  await expect(dialog.getByText(/freeze red line still go out/)).toBeVisible()
  await dialog.getByTestId('mute-database').fill('orders')
  await dialog.getByTestId('mute-class').selectOption('digest')
  await dialog.getByTestId('mute-duration').selectOption('168')
  await dialog.getByTestId('mute-reason').fill('vacuum noise')
  const before = Date.now()
  await dialog.getByTestId('mute-submit').click()
  await expect(dialog).toBeHidden()

  const post = sent.find(s => s.method === 'POST')!
  const body = post.body as Record<string, string>
  expect(body.database).toBe('orders')
  expect(body.class).toBe('digest')
  expect(body.reason).toBe('vacuum noise')
  expect(body.until).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/)
  const hours = (Date.parse(body.until) - before) / 3600000
  expect(hours).toBeGreaterThan(167.9)
  expect(hours).toBeLessThan(168.1)

  const muteRow = page.getByTestId('mute-row')
  await expect(muteRow).toContainText('orders - Digest')
  await expect(muteRow).toContainText('by test@test.com: vacuum noise')
  await muteRow.getByTestId('unmute-button').click()
  await expect(page.getByTestId('mutes-empty')).toBeVisible()
  const del = sent.find(s => s.method === 'DELETE')!
  expect(new URL(del.url).searchParams.get('database')).toBe('orders')
  expect(new URL(del.url).searchParams.get('class')).toBe('digest')
})

test('a mute link from a message footer opens the dialog filled in', async ({ page }) => {
  await mockAllAPIs(page)
  const sent = await mockQuietMute(page, { messages: 1, led_to_action: 0,
    text: '1 message, 0 led to an action' })
  await page.goto('/#/notifications?class=ask&mute=billing')
  const dialog = page.getByRole('dialog', { name: 'Mute database' })
  await expect(dialog.getByTestId('mute-database')).toHaveValue('billing')
  await expect(dialog.getByTestId('mute-class')).toHaveValue('ask')
  await dialog.getByTestId('mute-cancel').click()
  await expect(dialog).toBeHidden()
  expect(sent.filter(s => s.method === 'POST')).toHaveLength(0)
})
