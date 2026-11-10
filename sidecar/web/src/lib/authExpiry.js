// installAuthExpiryInterceptor wraps win.fetch so any 401 from the API
// (reads and mutations alike) dispatches sage:auth-expired and App
// returns to the login screen (G9-B21). Login and the /auth/me probe
// are excluded: a 401 there is an expected answer, not an expiry.
//
// A 503 with code auth_store_unavailable means the session store could not
// answer, not that the session ended (apiauth-02): it dispatches
// sage:auth-degraded, the user stays signed in, and the next successful API
// response dispatches sage:auth-restored.
export const AUTH_STORE_UNAVAILABLE = 'auth_store_unavailable'

export function installAuthExpiryInterceptor(win) {
  const original = win.fetch.bind(win)
  let degraded = false
  win.fetch = async (input, init) => {
    const res = await original(input, init)
    const url = typeof input === 'string' ? input : input?.url || ''
    if (!isAPI(url)) return res
    if (res.status === 401 && isExpiryCandidate(url)) {
      degraded = false
      win.dispatchEvent(new CustomEvent('sage:auth-expired'))
    } else if (res.status === 503 && await isAuthStoreUnavailable(res)) {
      degraded = true
      win.dispatchEvent(new CustomEvent('sage:auth-degraded'))
    } else if (res.ok && degraded) {
      degraded = false
      win.dispatchEvent(new CustomEvent('sage:auth-restored'))
    }
    return res
  }
}

// isAuthStoreUnavailable reads the code from a copy of the body, so the
// caller can still read the response.
export async function isAuthStoreUnavailable(res) {
  try {
    const copy = typeof res.clone === 'function' ? res.clone() : res
    const body = await copy.json()
    return body?.code === AUTH_STORE_UNAVAILABLE
  } catch {
    // A non-JSON 503 is some other outage, not the session store.
    return false
  }
}

function apiPath(url) {
  return url.replace(/^https?:\/\/[^/]+/, '')
}

function isAPI(url) {
  return apiPath(url).startsWith('/api/v1/')
}

function isExpiryCandidate(url) {
  return !apiPath(url).startsWith('/api/v1/auth/')
}
