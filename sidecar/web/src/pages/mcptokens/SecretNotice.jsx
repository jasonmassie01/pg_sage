// MCP v2: the one-time display of a new token's secret and the Claude Code
// setup line. Once dismissed, the secret is gone from the page for good.
import { useState } from 'react'
import { setupCommand } from './tokenApi'

function CopyButton({ testId, text, label }) {
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
    <button type="button" data-testid={testId} onClick={copy}
      className="px-2 py-1 rounded text-xs"
      style={{ color: 'var(--accent)', border: '1px solid var(--border)' }}>
      {state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy failed' : label}
    </button>
  )
}

function SecretLine({ testId, copyTestId, text, label }) {
  return (
    <div className="flex items-start gap-2 mb-2">
      <code data-testid={testId} className="block flex-1 break-all text-xs p-2 rounded"
        style={{ background: 'var(--bg-main)' }}>
        {text}
      </code>
      <CopyButton testId={copyTestId} text={text} label={label} />
    </div>
  )
}

export function SecretNotice({ created, onDismiss }) {
  if (!created) return null
  const command = setupCommand(window.location.origin, created.token)
  return (
    <div data-testid="mcp-token-secret" className="text-sm p-3 rounded mb-4"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--accent)',
        color: 'var(--text-primary)' }}>
      <p className="mb-2">
        Token <strong>{created.name}</strong> created. Copy the secret now: it will
        not be shown again.
      </p>
      <SecretLine testId="mcp-token-secret-value" copyTestId="mcp-token-copy-secret"
        text={created.token} label="Copy token" />
      <p className="mb-1 text-xs" style={{ color: 'var(--text-secondary)' }}>
        Claude Code setup (run once on the agent&apos;s machine):
      </p>
      <SecretLine testId="mcp-token-setup-command" copyTestId="mcp-token-copy-setup"
        text={command} label="Copy command" />
      <button type="button" data-testid="mcp-token-secret-dismiss" onClick={onDismiss}
        className="mt-1 text-xs underline" style={{ color: 'var(--text-secondary)' }}>
        I saved it, dismiss
      </button>
    </div>
  )
}
