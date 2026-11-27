// Where Slack or Telegram must send button presses for a channel that
// approves from chat (alerting-10). Read-only, with copy buttons; the
// Telegram setWebhook URL names the bot token and secret as placeholders.
import { useState } from 'react'
import { callbackUrl, needsCallback, telegramSetWebhookUrl } from './channelConfig'

function CopyLine({ testId, copyTestId, text }) {
  const [state, setState] = useState('idle')
  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setState('copied')
    } catch (err) {
      // The clipboard API is missing or blocked (insecure context); the
      // text stays selectable on the page.
      console.warn('copy to clipboard failed:', err)
      setState('failed')
    }
  }
  return (
    <div className="flex items-start gap-2 mb-2">
      <code data-testid={testId} className="block flex-1 break-all text-xs p-2 rounded"
        style={{ background: 'var(--bg-main)' }}>
        {text}
      </code>
      <button type="button" data-testid={copyTestId} onClick={copy}
        className="px-2 py-1 rounded text-xs"
        style={{ color: 'var(--accent)', border: '1px solid var(--border)' }}>
        {state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy failed' : 'Copy'}
      </button>
    </div>
  )
}

function TelegramNotes({ channel }) {
  const chat = channel.config?.chat_id ?? ''
  return (
    <>
      {!channel.config?.webhook_secret && (
        <p className="text-xs mb-1" style={{ color: 'var(--red)' }}>
          This channel has no webhook secret: it notifies but cannot approve.
        </p>
      )}
      {chat.startsWith('@') && (
        <p className="text-xs mb-1" style={{ color: 'var(--red)' }}>
          Approvals need a numeric chat ID; {chat} only receives notifications.
        </p>
      )}
    </>
  )
}

export function CallbackInfo({ channel }) {
  if (!needsCallback(channel)) return null
  const origin = window.location.origin
  const telegram = channel.type === 'telegram'
  const provider = telegram ? 'Telegram' : 'Slack'
  return (
    <div data-testid="callback-info" className="text-sm p-3 rounded mt-3"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--accent)',
        color: 'var(--text-primary)' }}>
      <p className="mb-2">
        {telegram
          ? 'Telegram sends button presses for this channel to:'
          : "Set the Slack app's Interactivity Request URL to:"}
      </p>
      <CopyLine testId="callback-url" copyTestId="callback-copy"
        text={callbackUrl(channel, origin)} />
      {telegram && (
        <>
          <p className="mb-1 text-xs" style={{ color: 'var(--text-secondary)' }}>
            Register it once: open this URL with the bot token and the webhook secret
            filled in.
          </p>
          <CopyLine testId="telegram-setwebhook-url" copyTestId="telegram-setwebhook-copy"
            text={telegramSetWebhookUrl(channel, origin)} />
          <TelegramNotes channel={channel} />
        </>
      )}
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        The URL must be reachable from {provider} over HTTPS. A chat user can approve only
        after an admin links them to a pg_sage user below.
      </p>
    </div>
  )
}
