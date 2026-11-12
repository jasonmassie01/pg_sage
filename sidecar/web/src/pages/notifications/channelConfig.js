// Channel settings shared by the channel form and the callback panel
// (alerting-10). The backend's rules live in internal/store
// (notification_validation.go, notification_chatops_validation.go).
import { parseError } from '../../lib/api'

export const CHANNELS_URL = '/api/v1/notifications/channels'

export const CHANNEL_TYPES = [
  ['slack', 'Slack'],
  ['telegram', 'Telegram'],
  ['email', 'Email'],
  ['pagerduty', 'PagerDuty'],
  ['webhook', 'Webhook'],
  ['teams', 'Microsoft Teams'],
  ['opsgenie', 'Opsgenie'],
  ['incidentio', 'incident.io'],
  ['jira', 'Jira (case tickets)'],
]

// The keys the backend reads for a channel that approves from chat; a
// plain Slack webhook never sends them.
const SLACK_CHATOPS_KEYS = ['interactive', 'signing_secret', 'team_id']

export function defaultConfig(type) {
  switch (type) {
    case 'telegram':
      return { bot_token: '', chat_id: '', webhook_secret: '' }
    case 'pagerduty':
      return { routing_key: '' }
    case 'webhook':
      return { webhook_url: '', headers: '' }
    case 'teams':
      return { webhook_url: '', dashboard_url: '' }
    case 'opsgenie':
      return { api_key: '', region: 'us' }
    case 'incidentio':
      return { alert_source_config_id: '', token: '' }
    case 'jira':
      return { base_url: '', email: '', api_token: '', project_key: '', issue_type: '' }
    case 'email':
      return { smtp_host: '', smtp_port: '587', smtp_user: '', smtp_pass: '', from: '', to: '' }
    default:
      return { webhook_url: '' }
  }
}

// The alphabet of Telegram's secret_token, 64 symbols, so a random byte
// masked to 6 bits picks each one with equal chance.
const SECRET_ALPHABET =
  'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-'

export function generateWebhookSecret() {
  const bytes = crypto.getRandomValues(new Uint8Array(32))
  return Array.from(bytes, b => SECRET_ALPHABET[b & 63]).join('')
}

export function isInteractive(config) {
  return config?.interactive === 'true'
}

// needsCallback: the channel can approve from chat, so its provider must
// be told where to send button presses.
export function needsCallback(channel) {
  if (!channel) return false
  if (channel.type === 'telegram') return true
  return channel.type === 'slack' && isInteractive(channel.config)
}

export function callbackUrl(channel, origin) {
  if (channel.type !== 'slack' && channel.type !== 'telegram') return null
  return `${origin}/api/v1/chatops/${channel.type}/${channel.id}`
}

// telegramSetWebhookUrl never carries the bot token or the secret: the
// operator fills both in, so neither reaches the clipboard or the screen.
export function telegramSetWebhookUrl(channel, origin) {
  return 'https://api.telegram.org/bot<bot_token>/setWebhook?url=' +
    encodeURIComponent(callbackUrl(channel, origin)) +
    '&secret_token=<webhook_secret>'
}

// payloadConfig drops empty values (the backend treats them as unset).
// A new plain Slack channel sends no ChatOps keys; an edit keeps them, so
// turning interactivity off keeps the stored signing secret.
export function payloadConfig(type, config, editing) {
  const out = {}
  for (const [key, value] of Object.entries(config)) {
    if (value !== '' && value !== undefined && value !== null) out[key] = value
  }
  if (type === 'slack' && !editing && !isInteractive(out)) {
    for (const key of SLACK_CHATOPS_KEYS) delete out[key]
  }
  return out
}

// clientError is the check the browser makes before sending; everything
// else is the API's call, and its message is shown as is.
export function clientError(type, config) {
  if (type === 'slack' && isInteractive(config) && !config.signing_secret?.trim()) {
    return "Interactive approvals need the Slack app's signing secret."
  }
  return null
}

// failureMessage is the API's own error text, or what failed and the
// status when the body carries none.
export async function failureMessage(res, what) {
  const err = await parseError(res)
  return err.serverMessage || `${what} (${res.status})`
}
