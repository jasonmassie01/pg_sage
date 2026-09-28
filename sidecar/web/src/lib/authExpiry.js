// installAuthExpiryInterceptor wraps win.fetch so any 401 from the API
// (reads and mutations alike) dispatches sage:auth-expired and App
// returns to the login screen (G9-B21). Login and the /auth/me probe
// are excluded: a 401 there is an expected answer, not an expiry.
export function installAuthExpiryInterceptor(win) {
  const original = win.fetch.bind(win)
  win.fetch = async (input, init) => {
    const res = await original(input, init)
    const url = typeof input === 'string' ? input : input?.url || ''
    if (res.status === 401 && isExpiryCandidate(url)) {
      win.dispatchEvent(new CustomEvent('sage:auth-expired'))
    }
    return res
  }
}

function isExpiryCandidate(url) {
  const path = url.replace(/^https?:\/\/[^/]+/, '')
  return path.startsWith('/api/v1/') && !path.startsWith('/api/v1/auth/')
}
