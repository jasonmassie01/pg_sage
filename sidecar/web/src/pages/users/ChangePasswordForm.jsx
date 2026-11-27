import { useState } from 'react'
import { changePassword, passwordProblem } from './passwordApi'

// apiauth-03: change the signed-in user's password. The current password is
// required; other sessions end, this one stays signed in. `forced` is the
// first sign-in after a reset or a generated bootstrap password.

const inputStyle = {
  background: 'var(--bg-main)',
  border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}

function Field({ label, testId, value, onChange, autoComplete }) {
  return (
    <label className="block text-xs mb-2" style={{ color: 'var(--text-secondary)' }}>
      {label}
      <input type="password" value={value} required data-testid={testId}
        autoComplete={autoComplete} onChange={e => onChange(e.target.value)}
        className="block w-full max-w-sm mt-1 px-3 py-1.5 rounded text-sm"
        style={inputStyle} />
    </label>
  )
}

function endedText(count) {
  if (!count) return 'Password changed.'
  return `Password changed. ${count} other sessions were signed out.`
}

export function ChangePasswordForm({ user, forced = false, onChanged }) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState(null)
  const [done, setDone] = useState(null)
  const [busy, setBusy] = useState(false)

  async function handleSubmit(e) {
    e.preventDefault()
    setDone(null)
    const problem = passwordProblem(user?.email, next, confirm)
    if (problem) {
      setError(problem)
      return
    }
    setError(null)
    setBusy(true)
    try {
      const data = await changePassword(current, next)
      setCurrent('')
      setNext('')
      setConfirm('')
      setDone(endedText(data.sessions_ended))
      onChanged?.()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form data-testid="change-password-form" onSubmit={handleSubmit} className="mt-4">
      <h3 className="text-sm font-semibold mb-2" style={{ color: 'var(--text-primary)' }}>
        {forced ? 'Choose a new password' : 'Change password'}
      </h3>
      <p className="text-xs mb-3" style={{ color: 'var(--text-secondary)' }}>
        12 to 72 bytes, not your email address. Your other sessions are signed out.
      </p>
      <Field label={forced ? 'Temporary password' : 'Current password'}
        testId="current-password" value={current} onChange={setCurrent}
        autoComplete="current-password" />
      <Field label="New password" testId="new-password" value={next} onChange={setNext}
        autoComplete="new-password" />
      <Field label="Confirm new password" testId="confirm-password" value={confirm}
        onChange={setConfirm} autoComplete="new-password" />
      {error && (
        <div data-testid="change-password-error" role="alert"
          className="text-sm p-2 rounded mb-2"
          style={{ color: '#ef4444', border: '1px solid rgba(239,68,68,0.3)' }}>
          {error}
        </div>
      )}
      {done && (
        <div data-testid="change-password-success" className="text-sm p-2 rounded mb-2"
          style={{ color: '#16a34a', border: '1px solid rgba(22,163,74,0.3)' }}>
          {done}
        </div>
      )}
      <button type="submit" disabled={busy} data-testid="change-password-submit"
        className="px-4 py-1.5 rounded text-sm font-medium"
        style={{ background: 'var(--accent-solid)', color: '#fff', opacity: busy ? 0.6 : 1 }}>
        {busy ? 'Saving...' : 'Change password'}
      </button>
    </form>
  )
}
