/* eslint-disable react-refresh/only-export-components */
// parts: what the setup wizard's steps share (ux-04b).

export const JSON_HEADERS = { 'Content-Type': 'application/json' }

// requestJSON sends one request and returns { ok, status, body }; a body
// that is not JSON reads as null. A network failure rejects, as fetch does.
export async function requestJSON(url, init = {}) {
  const res = await fetch(url, { credentials: 'include', ...init })
  const body = await res.json().catch(() => null)
  return { ok: res.ok, status: res.status, body }
}

export function putJSON(url, body) {
  return requestJSON(url, { method: 'PUT', headers: JSON_HEADERS, body: JSON.stringify(body) })
}

export function postJSON(url, body) {
  return requestJSON(url, { method: 'POST', headers: JSON_HEADERS,
    body: body === undefined ? undefined : JSON.stringify(body) })
}

// STALE_WRITE is what a 409 from the config generation check means here.
export const STALE_WRITE = 'Another admin changed the configuration first; it was reread, ' +
  'so check the values and try again.'

export const cardStyle = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
export const inputStyle = {
  background: 'var(--bg-main)', border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}

export function SkipButton({ step, onSkip }) {
  return (
    <button type="button" onClick={() => onSkip(step)} className="px-3 py-1.5 rounded text-sm"
      style={{ color: 'var(--text-secondary)', border: '1px solid var(--border)' }}>
      Skip this step
    </button>
  )
}

export function PrimaryButton({ testId, disabled, onClick, children }) {
  return (
    <button type="button" data-testid={testId} disabled={disabled} onClick={onClick}
      className="px-4 py-1.5 rounded text-sm font-medium disabled:opacity-50"
      style={{ background: 'var(--accent)', color: '#fff' }}>
      {children}
    </button>
  )
}

// YamlBlock is the configuration to paste where the file owns it (the
// read-only rule): it never carries a secret, only a placeholder.
export function YamlBlock({ intro, yaml }) {
  return (
    <div className="space-y-1">
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>{intro}</p>
      <pre data-testid="setup-yaml" className="rounded p-3 text-xs font-mono whitespace-pre-wrap"
        style={{ background: 'var(--bg-primary)', border: '1px solid var(--border)',
          color: 'var(--text-primary)' }}>
        {yaml}
      </pre>
    </div>
  )
}

export function Notice({ testId, ok, children }) {
  return (
    <p data-testid={testId} role={ok ? 'status' : 'alert'} className="text-sm"
      style={{ color: ok ? 'var(--green)' : 'var(--red, #ef4444)' }}>
      {children}
    </p>
  )
}

export function TextField({ label, testId, type = 'text', value, onChange, placeholder }) {
  return (
    <label className="block text-xs space-y-1" style={{ color: 'var(--text-secondary)' }}>
      <span>{label}</span>
      <input type={type} value={value} data-testid={testId} placeholder={placeholder}
        onChange={e => onChange(e.target.value)} autoComplete="off" spellCheck={false}
        className="w-full px-3 py-1.5 rounded text-sm" style={inputStyle} />
    </label>
  )
}
