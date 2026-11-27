// notifications-chatops.spec.ts — alerting-10: set up approve-from-chat in
// the Channels tab. Creates a Telegram channel, copies its callback URL,
// edits it (the masked secrets go back unchanged), and links a chat user.
// Every /api route is mocked.

import { test, expect, type Page, type Route } from '@playwright/test'
import { mockAllAPIs } from './fixtures'

type Channel = {
  id: number; name: string; type: string; enabled: boolean
  config: Record<string, string>
  last_status: string; last_error: string; last_sent_at: string | null
}

async function mockChatOps(page: Page) {
  const channels: Channel[] = []
  const identities: Record<string, unknown>[] = []
  const sent: { method: string; body: unknown }[] = []
  await page.route('**/api/v1/notifications/channels**', async (route: Route) => {
    const req = route.request()
    const method = req.method()
    if (method === 'GET') return route.fulfill({ json: { channels } })
    const body = req.postDataJSON()
    sent.push({ method, body })
    if (method === 'POST') {
      channels.push({ id: 12, name: body.name, type: body.type, enabled: true,
        config: { ...body.config, bot_token: '1234****-def', webhook_secret: '****' },
        last_status: '', last_error: '', last_sent_at: null })
      return route.fulfill({ status: 201, json: { id: 12, name: body.name, type: body.type } })
    }
    channels[0].name = body.name
    return route.fulfill({ json: { ok: true } })
  })
  await page.route('**/api/v1/users', route => route.fulfill({ json: { users: [
    { id: 7, email: 'ops@example.com', role: 'operator' },
  ] } }))
  await page.route('**/api/v1/chatops/identities**', async (route: Route) => {
    const req = route.request()
    if (req.method() === 'GET') return route.fulfill({ json: { items: identities } })
    const body = req.postDataJSON()
    sent.push({ method: req.method(), body })
    identities.push({ id: 1, ...body, user_email: 'ops@example.com',
      created_at: '2030-01-01T00:00:00Z' })
    return route.fulfill({ status: 201, json: identities[0] })
  })
  return sent
}

test('sets up Telegram approvals from the Channels tab', async ({ page, context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  await mockAllAPIs(page)
  const sent = await mockChatOps(page)
  await page.goto('/#/notifications')

  await page.getByTestId('add-channel-name').fill('ops-telegram')
  await page.getByTestId('add-channel-type').selectOption('telegram')
  await page.getByTestId('add-channel-bot-token').fill('123456:ABC-def')
  await page.getByTestId('add-channel-chat-id').fill('-1001234')
  await page.getByTestId('add-channel-generate-secret').click()
  const secret = await page.getByTestId('add-channel-webhook-secret').inputValue()
  expect(secret).toMatch(/^[A-Za-z0-9_-]{32}$/)
  await page.getByTestId('add-channel-submit').click()

  const origin = new URL(page.url()).origin
  const callback = `${origin}/api/v1/chatops/telegram/12`
  await expect(page.getByTestId('callback-url')).toHaveText(callback)
  await expect(page.getByTestId('telegram-setwebhook-url'))
    .toContainText('secret_token=<webhook_secret>')
  await page.getByTestId('callback-copy').click()
  await expect(page.getByTestId('callback-copy')).toHaveText('Copied')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(callback)
  expect(sent[0]).toEqual({ method: 'POST', body: { name: 'ops-telegram', type: 'telegram',
    config: { bot_token: '123456:ABC-def', chat_id: '-1001234', webhook_secret: secret } } })

  const row = page.getByTestId('channel-row').filter({ hasText: 'ops-telegram' })
  await expect(row.getByTestId('channel-last-delivery')).toHaveText(/never delivered/)
  await row.getByTestId('channel-edit-button').click()
  const dialog = page.getByRole('dialog', { name: /edit channel/i })
  await dialog.getByTestId('edit-channel-name').fill('ops-telegram-2')
  await dialog.getByTestId('edit-channel-submit').click()
  await expect(dialog).toHaveCount(0)
  expect(sent[1]).toEqual({ method: 'PUT', body: { name: 'ops-telegram-2', enabled: true,
    config: { bot_token: '1234****-def', chat_id: '-1001234', webhook_secret: '****' } } })
  await expect(page.getByTestId('channel-row')).toContainText('ops-telegram-2')

  const panel = page.getByTestId('chat-identities')
  await panel.getByTestId('chat-identity-provider').selectOption('telegram')
  await panel.getByTestId('chat-identity-external-user-id').fill('4242')
  await panel.getByTestId('chat-identity-user').selectOption('7')
  await panel.getByTestId('chat-identity-link').click()
  await expect(panel.getByTestId('chat-identity-row')).toContainText('4242')
  expect(sent[2]).toEqual({ method: 'POST',
    body: { provider: 'telegram', external_user_id: '4242', user_id: 7 } })
})
