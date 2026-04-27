import { useState, useCallback } from 'react'
import { useAPI } from '../hooks/useAPI'
import { AlertTriangle, RotateCcw } from 'lucide-react'

function formatTokens(n) {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}K`
  return String(n)
}

function formatResetTime(iso) {
  if (!iso) return null
  try {
    const d = new Date(iso)
    return d.toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
    })
  } catch {
    return iso
  }
}

function ClientLine({ label, client }) {
  if (!client?.budget_exhausted) return null
  const pct = client.token_budget > 0
    ? Math.round((client.tokens_used / client.token_budget) * 100)
    : 100
  const resetStr = formatResetTime(client.resets_at)
  return (
    <div className="flex items-center gap-2 text-sm">
      <span className="font-medium">{label}:</span>
      <span>
        {formatTokens(client.tokens_used)}
        {' / '}
        {formatTokens(client.token_budget)}
        {' '}({pct}%)
      </span>
      {resetStr && (
        <span style={{ color: 'var(--text-secondary)' }}>
          — resets at {resetStr}
        </span>
      )}
    </div>
  )
}

export function TokenBudgetBanner() {
  const { data, refetch } = useAPI('/api/v1/llm/status', 30000)
  const [resetting, setResetting] = useState(false)
  const [resetError, setResetError] = useState(null)

  const handleReset = useCallback(async () => {
    setResetting(true)
    setResetError(null)
    try {
      const res = await fetch('/api/v1/llm/budget/reset', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
      })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw new Error(body.error || `HTTP ${res.status}`)
      }
      refetch()
    } catch (e) {
      setResetError(e.message)
    } finally {
      setResetting(false)
    }
  }, [refetch])

  if (!data) return null

  const generalExhausted = data.general?.budget_exhausted === true
  const optimizerExhausted = data.optimizer?.budget_exhausted === true

  if (!generalExhausted && !optimizerExhausted) return null

  return (
    <div
      data-testid="token-budget-banner"
      className="rounded p-4 flex items-start gap-3"
      style={{
        background: 'rgba(251, 191, 36, 0.08)',
        border: '1px solid rgba(251, 191, 36, 0.3)',
      }}
    >
      <AlertTriangle
        size={20}
        className="flex-shrink-0 mt-0.5"
        style={{ color: 'var(--yellow)' }}
      />
      <div className="flex-1 min-w-0">
        <div
          className="font-semibold mb-1"
          style={{ color: 'var(--yellow)' }}
        >
          LLM token budget exhausted
        </div>
        {generalExhausted && (
          <ClientLine label="General" client={data.general} />
        )}
        {optimizerExhausted && (
          <ClientLine label="Optimizer" client={data.optimizer} />
        )}
        {resetError && (
          <div
            className="text-xs mt-1"
            style={{ color: 'var(--red)' }}
          >
            Reset failed: {resetError}
          </div>
        )}
      </div>
      <button
        data-testid="token-budget-reset"
        onClick={handleReset}
        disabled={resetting}
        className="flex items-center gap-1.5 px-3 py-1.5 rounded
          text-xs font-medium flex-shrink-0"
        style={{
          background: 'var(--bg-card)',
          color: 'var(--yellow)',
          border: '1px solid rgba(251, 191, 36, 0.3)',
          opacity: resetting ? 0.6 : 1,
        }}
      >
        <RotateCcw size={14} />
        {resetting ? 'Resetting...' : 'Reset Budget'}
      </button>
    </div>
  )
}
