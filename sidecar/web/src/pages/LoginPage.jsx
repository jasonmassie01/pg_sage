import { useState, useEffect } from 'react'

const providerLabels = {
  google: 'Google',
  github: 'GitHub',
  oidc: 'SSO',
}

export function LoginPage({ onLogin }) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(false)
  const [oauthConfig, setOauthConfig] = useState(null)
  const [oauthLoading, setOauthLoading] = useState(false)

  useEffect(() => {
    fetch('/api/v1/auth/oauth/config')
      .then(res => res.ok ? res.json() : null)
      .then(data => setOauthConfig(data))
      .catch(() => setOauthConfig(null))
  }, [])

  async function handleSubmit(e) {
    e.preventDefault()
    setError(null)
    setLoading(true)
    try {
      const res = await fetch('/api/v1/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({ email, password }),
      })
      if (!res.ok) {
        const data = await res.json()
        throw new Error(data.error || 'Login failed')
      }
      const user = await res.json()
      onLogin(user)
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }

  async function handleOAuthLogin() {
    setError(null)
    setOauthLoading(true)
    try {
      const res = await fetch('/api/v1/auth/oauth/authorize', {
        credentials: 'include',
      })
      if (!res.ok) {
        const data = await res.json()
        throw new Error(data.error || 'OAuth unavailable')
      }
      const { url } = await res.json()
      window.location.href = url
    } catch (err) {
      setError(err.message)
      setOauthLoading(false)
    }
  }

  const providerLabel = oauthConfig?.provider
    ? (providerLabels[oauthConfig.provider] || oauthConfig.provider)
    : 'SSO'

  return (
    <div className="flex items-center justify-center min-h-screen"
      style={{ background: 'var(--bg-main)' }}>
      <div className="w-full max-w-sm p-8 rounded-lg shadow-lg"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h1 className="text-2xl font-bold mb-6 text-center"
          style={{ color: 'var(--accent)' }}>
          pg_sage
        </h1>
        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          {error && (
            <div className="text-sm p-3 rounded"
              data-testid="login-error"
              style={{
                background: 'rgba(239,68,68,0.1)',
                color: '#ef4444',
                border: '1px solid rgba(239,68,68,0.3)',
              }}>
              {error}
            </div>
          )}
          <div>
            <label className="block text-sm mb-1"
              style={{ color: 'var(--text-secondary)' }}>
              Email
            </label>
            <input
              type="text"
              data-testid="login-email"
              value={email}
              onChange={e => setEmail(e.target.value)}
              required
              className="w-full px-3 py-2 rounded text-sm"
              style={{
                background: 'var(--bg-main)',
                border: '1px solid var(--border)',
                color: 'var(--text-primary)',
              }}
            />
          </div>
          <div>
            <label className="block text-sm mb-1"
              style={{ color: 'var(--text-secondary)' }}>
              Password
            </label>
            <input
              type="password"
              data-testid="login-password"
              value={password}
              onChange={e => setPassword(e.target.value)}
              required
              className="w-full px-3 py-2 rounded text-sm"
              style={{
                background: 'var(--bg-main)',
                border: '1px solid var(--border)',
                color: 'var(--text-primary)',
              }}
            />
          </div>
          <button
            type="submit"
            data-testid="login-submit"
            disabled={loading}
            className="w-full py-2 rounded text-sm font-medium"
            style={{
              background: 'var(--accent)',
              color: '#fff',
              opacity: loading ? 0.6 : 1,
            }}>
            {loading ? 'Signing in...' : 'Sign In'}
          </button>
        </form>

        {oauthConfig?.enabled && (
          <>
            <div className="flex items-center gap-3 my-4">
              <div className="flex-1 h-px"
                style={{ background: 'var(--border)' }} />
              <span className="text-xs"
                style={{ color: 'var(--text-secondary)' }}>or</span>
              <div className="flex-1 h-px"
                style={{ background: 'var(--border)' }} />
            </div>
            <button
              onClick={handleOAuthLogin}
              data-testid="oauth-login"
              disabled={oauthLoading}
              className="w-full py-2 rounded text-sm font-medium"
              style={{
                background: 'transparent',
                color: 'var(--text-primary)',
                border: '1px solid var(--border)',
                opacity: oauthLoading ? 0.6 : 1,
              }}>
              {oauthLoading
                ? 'Redirecting...'
                : `Sign in with ${providerLabel}`}
            </button>
          </>
        )}
      </div>
    </div>
  )
}
