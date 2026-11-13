// The per-type settings of a notification channel (alerting-10). Secret
// fields are password inputs; in an edit they hold the API's masked
// value, which the store swaps back for the stored secret.
import { useState } from 'react'
import { inputStyle, FormField } from './shared'
import { generateWebhookSecret, isInteractive } from './channelConfig'

const FIELDS = {
  slack: [
    { key: 'webhook_url', label: 'Webhook URL', required: true, wide: true,
      placeholder: 'https://hooks.slack.com/...' },
  ],
  pagerduty: [
    { key: 'routing_key', label: 'Routing Key', required: true, wide: true,
      placeholder: 'PagerDuty routing key' },
  ],
  email: [
    { key: 'smtp_host', label: 'SMTP Host', required: true },
    { key: 'smtp_port', label: 'Port', narrow: true },
    { key: 'smtp_user', label: 'User' },
    { key: 'smtp_pass', label: 'Password', secret: true },
    { key: 'from', label: 'From', required: true },
    { key: 'to', label: 'To (comma-separated)', required: true, wide: true },
  ],
  webhook: [
    { key: 'webhook_url', label: 'Webhook URL', required: true, wide: true,
      placeholder: 'https://hooks.example.com/...' },
    { key: 'headers', label: 'Headers (JSON object)', secret: true, wide: true,
      placeholder: '{"Authorization": "Bearer ..."}' },
  ],
  // market-14: Teams Workflows webhook, Opsgenie Alert API, incident.io
  // HTTP alert source.
  teams: [
    { key: 'webhook_url', label: 'Workflows webhook URL', required: true, secret: true,
      wide: true, placeholder: 'https://prod-00.westus.logic.azure.com/workflows/...' },
    { key: 'dashboard_url', label: 'pg_sage link (optional)', wide: true,
      placeholder: 'https://pg-sage.example.com' },
  ],
  opsgenie: [
    { key: 'api_key', label: 'API key', required: true, secret: true, wide: true },
    { key: 'region', label: 'Region', options: [['us', 'US'], ['eu', 'EU']] },
  ],
  incidentio: [
    { key: 'alert_source_config_id', label: 'Alert source config ID', required: true,
      placeholder: '01H...' },
    { key: 'token', label: 'API token', required: true, secret: true, wide: true },
  ],
  // A case ticket integration, not an alert destination: no rule routes
  // events to it; operators create tickets from cases.
  jira: [
    { key: 'base_url', label: 'Jira site URL', required: true, wide: true,
      placeholder: 'https://acme.atlassian.net' },
    { key: 'email', label: 'Account email', required: true },
    { key: 'api_token', label: 'API token', required: true, secret: true, wide: true },
    { key: 'project_key', label: 'Project key', required: true, narrow: true,
      placeholder: 'OPS' },
    { key: 'issue_type', label: 'Issue type', placeholder: 'Task' },
  ],
  telegram: [
    { key: 'bot_token', label: 'Bot token', required: true, secret: true, wide: true,
      placeholder: '123456:ABC...' },
    { key: 'chat_id', label: 'Chat ID', required: true,
      placeholder: '-1001234567890 or @channel' },
  ],
  slackChatOps: [
    { key: 'signing_secret', label: 'Signing secret', secret: true, wide: true },
    { key: 'team_id', label: 'Team ID (accept only this workspace)', placeholder: 'T0123' },
  ],
}

function testId(prefix, key) {
  return `${prefix}-${key.replace(/_/g, '-')}`
}

function widthClass(f) {
  if (f.wide) return 'w-80'
  return f.narrow ? 'w-20' : ''
}

function ConfigSelect({ prefix, field, config, upd }) {
  return (
    <FormField label={field.label}>
      <select value={config[field.key] || field.options[0][0]} aria-label={field.label}
        data-testid={testId(prefix, field.key)}
        onChange={e => upd(field.key, e.target.value)}
        className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
        {field.options.map(([value, label]) => (
          <option key={value} value={value}>{label}</option>
        ))}
      </select>
    </FormField>
  )
}

function ConfigInput({ prefix, field, config, upd }) {
  if (field.options) {
    return <ConfigSelect prefix={prefix} field={field} config={config} upd={upd} />
  }
  return (
    <FormField label={field.label}>
      <input value={config[field.key] ?? ''} required={field.required}
        type={field.secret ? 'password' : 'text'} autoComplete="off"
        data-testid={testId(prefix, field.key)} aria-label={field.label}
        onChange={e => upd(field.key, e.target.value)}
        className={`px-3 py-1.5 rounded text-sm ${widthClass(field)}`}
        style={inputStyle} placeholder={field.placeholder} />
    </FormField>
  )
}

function Help({ children }) {
  return (
    <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>{children}</p>
  )
}

function WebhookSecret({ prefix, config, upd }) {
  const [shown, setShown] = useState(false)
  return (
    <FormField label="Webhook secret">
      <div className="flex gap-2">
        <input value={config.webhook_secret ?? ''} autoComplete="off"
          type={shown ? 'text' : 'password'} aria-label="Webhook secret"
          data-testid={testId(prefix, 'webhook_secret')}
          onChange={e => upd('webhook_secret', e.target.value)}
          className="px-3 py-1.5 rounded text-sm w-80" style={inputStyle} />
        <button type="button" data-testid={`${prefix}-generate-secret`}
          onClick={() => { upd('webhook_secret', generateWebhookSecret()); setShown(true) }}
          className="px-2 py-1 rounded text-xs" style={{ color: 'var(--accent)' }}>
          Generate
        </button>
        <button type="button" data-testid={`${prefix}-show-secret`}
          onClick={() => setShown(s => !s)}
          className="px-2 py-1 rounded text-xs" style={{ color: 'var(--text-secondary)' }}>
          {shown ? 'Hide' : 'Show'}
        </button>
      </div>
    </FormField>
  )
}

function TelegramFields({ prefix, config, upd }) {
  return (
    <>
      <div className="flex gap-3 flex-wrap">
        {FIELDS.telegram.map(f => (
          <ConfigInput key={f.key} prefix={prefix} field={f} config={config} upd={upd} />
        ))}
        <WebhookSecret prefix={prefix} config={config} upd={upd} />
      </div>
      <Help>
        Without a webhook secret the channel notifies but cannot approve. Approving
        also needs a numeric chat ID; an @channel name only notifies.
      </Help>
    </>
  )
}

function SlackFields({ prefix, config, upd }) {
  const interactive = isInteractive(config)
  return (
    <>
      <div className="flex gap-3 flex-wrap">
        <ConfigInput prefix={prefix} field={FIELDS.slack[0]} config={config} upd={upd} />
      </div>
      <label className="flex items-center gap-2 text-sm"
        style={{ color: 'var(--text-primary)' }}>
        <input type="checkbox" checked={interactive}
          data-testid={`${prefix}-interactive`}
          onChange={e => upd('interactive', e.target.checked ? 'true' : 'false')} />
        Interactive approvals (Approve and Deny buttons)
      </label>
      {interactive && (
        <div className="flex gap-3 flex-wrap">
          {FIELDS.slackChatOps.map(f => (
            <ConfigInput key={f.key} prefix={prefix} field={f} config={config} upd={upd} />
          ))}
        </div>
      )}
    </>
  )
}

export function ChannelFields({ prefix, type, config, setConfig }) {
  function upd(key, value) {
    setConfig(prev => ({ ...prev, [key]: value }))
  }
  if (type === 'slack') return <SlackFields prefix={prefix} config={config} upd={upd} />
  if (type === 'telegram') return <TelegramFields prefix={prefix} config={config} upd={upd} />
  return (
    <div className="flex gap-3 flex-wrap">
      {(FIELDS[type] || []).map(f => (
        <ConfigInput key={f.key} prefix={prefix} field={f} config={config} upd={upd} />
      ))}
    </div>
  )
}
