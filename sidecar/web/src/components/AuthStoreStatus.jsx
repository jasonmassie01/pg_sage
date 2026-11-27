import { useEffect } from 'react'

// apiauth-02: the session store (the control database) could not answer.
// The session is not known to be over, so the dashboard keeps the operator
// signed in and retries instead of showing the login page.

// Matches the Retry-After the API sends with auth_store_unavailable.
export const AUTH_STORE_RETRY_MS = 5000

// AuthStoreRetry is the boot screen when /auth/me answered 503
// auth_store_unavailable: it retries on its own every AUTH_STORE_RETRY_MS.
export function AuthStoreRetry({ onRetry }) {
  useEffect(() => {
    const timer = setTimeout(onRetry, AUTH_STORE_RETRY_MS)
    return () => clearTimeout(timer)
  }, [onRetry])
  return (
    <div role="alert" data-testid="auth-store-retry" className="p-6">
      <h1 className="text-lg font-semibold mb-2">Session store unavailable</h1>
      <p className="mb-4">
        pg_sage cannot reach its control database to check your sign-in, so
        it is retrying every {AUTH_STORE_RETRY_MS / 1000} seconds. You are not
        signed out.
      </p>
      <button type="button" onClick={onRetry} className="px-4 py-2 rounded"
        style={{ background: 'var(--accent-solid)', color: '#fff' }}>
        Retry now
      </button>
    </div>
  )
}

// AuthStoreBanner shows while signed-in requests answer 503
// auth_store_unavailable; the pages keep polling and it clears on the
// first successful API response.
export function AuthStoreBanner() {
  return (
    <div role="status" data-testid="auth-store-banner"
      className="fixed top-0 inset-x-0 z-50 px-4 py-2 text-sm text-center"
      style={{ background: 'var(--warning, #b45309)', color: '#fff' }}>
      Session store unavailable, retrying. You are still signed in.
    </div>
  )
}
