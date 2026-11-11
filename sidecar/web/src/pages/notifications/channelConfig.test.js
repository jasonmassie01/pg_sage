import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  callbackUrl, generateWebhookSecret, needsCallback, telegramSetWebhookUrl,
} from './channelConfig'

// alerting-10: the webhook secret is random from crypto.getRandomValues
// and fits the backend's pattern; callback URLs name the channel and never
// carry a secret.
// No concurrent access tests: pure functions.

afterEach(() => vi.restoreAllMocks())

describe('generateWebhookSecret', () => {
  it('returns 32 characters from the Telegram secret alphabet', () => {
    for (let i = 0; i < 50; i++) {
      expect(generateWebhookSecret()).toMatch(/^[A-Za-z0-9_-]{32}$/)
    }
  })

  it('draws from crypto.getRandomValues', () => {
    const spy = vi.spyOn(globalThis.crypto, 'getRandomValues')
    const a = generateWebhookSecret()
    const b = generateWebhookSecret()
    expect(spy).toHaveBeenCalledTimes(2)
    expect(a).not.toBe(b)
  })

  it('maps byte values onto the whole alphabet, ends included', () => {
    vi.spyOn(globalThis.crypto, 'getRandomValues').mockImplementation(arr => {
      arr.fill(0)
      arr[1] = 63
      arr[2] = 255
      return arr
    })
    const s = generateWebhookSecret()
    expect(s[0]).toBe('A')
    expect(s[1]).toBe('-')
    expect(s[2]).toBe('-')
  })
})

describe('callback URLs', () => {
  const origin = 'https://sage.example.com'

  it('builds the Slack interactivity URL for the channel', () => {
    expect(callbackUrl({ id: 5, type: 'slack' }, origin))
      .toBe('https://sage.example.com/api/v1/chatops/slack/5')
  })

  it('builds the Telegram callback and setWebhook URL with placeholders', () => {
    const ch = { id: 12, type: 'telegram',
      config: { bot_token: '123:real-token', webhook_secret: 'real-secret' } }
    expect(callbackUrl(ch, origin)).toBe('https://sage.example.com/api/v1/chatops/telegram/12')
    const set = telegramSetWebhookUrl(ch, origin)
    expect(set).toBe('https://api.telegram.org/bot<bot_token>/setWebhook?url=' +
      encodeURIComponent('https://sage.example.com/api/v1/chatops/telegram/12') +
      '&secret_token=<webhook_secret>')
    expect(set).not.toContain('real-token')
    expect(set).not.toContain('real-secret')
  })

  it('has no callback for channels that cannot approve', () => {
    expect(callbackUrl({ id: 1, type: 'email' }, origin)).toBeNull()
    expect(callbackUrl({ id: 1, type: 'pagerduty' }, origin)).toBeNull()
    expect(needsCallback({ type: 'slack', config: {} })).toBe(false)
    expect(needsCallback({ type: 'slack', config: { interactive: 'false' } })).toBe(false)
    expect(needsCallback({ type: 'slack', config: { interactive: 'true' } })).toBe(true)
    expect(needsCallback({ type: 'telegram', config: {} })).toBe(true)
    expect(needsCallback({ type: 'email' })).toBe(false)
    expect(needsCallback(null)).toBe(false)
  })
})
