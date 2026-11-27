import { describe, expect, it, vi } from 'vitest'
import { installAuthExpiryInterceptor } from './authExpiry'

// apiauth-02: only a 401 means the session ended. A 503 with the
// auth_store_unavailable code means the session store blinked: the user
// stays signed in and the app shows a degraded banner until an API call
// succeeds again.
// No concurrent access tests: the interceptor runs on the single-threaded
// browser event loop; ordering is covered by the sequential cases below.

const storeDown = () => ({
  ok: false, status: 503,
  clone() { return this },
  json: async () => ({ error: 'session store unavailable', code: 'auth_store_unavailable' }),
})
const ok = () => ({ ok: true, status: 200, json: async () => ({}) })

function harness(responses) {
  const target = new EventTarget()
  const base = vi.fn()
  for (const r of responses) base.mockResolvedValueOnce(r)
  const win = { fetch: base, dispatchEvent: e => target.dispatchEvent(e) }
  const seen = { expired: vi.fn(), degraded: vi.fn(), restored: vi.fn() }
  target.addEventListener('sage:auth-expired', seen.expired)
  target.addEventListener('sage:auth-degraded', seen.degraded)
  target.addEventListener('sage:auth-restored', seen.restored)
  installAuthExpiryInterceptor(win)
  return { win, seen }
}

describe('authExpiry interceptor', () => {
  it('fires sage:auth-expired on a 401 and nothing else', async () => {
    const { win, seen } = harness([{ ok: false, status: 401 }])
    const res = await win.fetch('/api/v1/findings')
    expect(res.status).toBe(401)
    expect(seen.expired).toHaveBeenCalledTimes(1)
    expect(seen.degraded).not.toHaveBeenCalled()
  })

  it('does not fire sage:auth-expired on a store-unavailable 503', async () => {
    const { win, seen } = harness([storeDown()])
    const res = await win.fetch('/api/v1/findings')
    expect(res.status).toBe(503)
    expect(seen.expired).not.toHaveBeenCalled()
    expect(seen.degraded).toHaveBeenCalledTimes(1)
  })

  it('ignores a 503 without the auth code (an ordinary outage of one endpoint)', async () => {
    const other = { ok: false, status: 503, json: async () => ({ error: 'llm down' }) }
    const { win, seen } = harness([other])
    await win.fetch('/api/v1/ask')
    expect(seen.expired).not.toHaveBeenCalled()
    expect(seen.degraded).not.toHaveBeenCalled()
  })

  it('tolerates a 503 whose body is not JSON', async () => {
    const broken = { ok: false, status: 503, json: async () => { throw new SyntaxError('x') } }
    const { win, seen } = harness([broken])
    const res = await win.fetch('/api/v1/findings')
    expect(res).toBe(broken)
    expect(seen.degraded).not.toHaveBeenCalled()
  })

  it('leaves the original body readable for the caller', async () => {
    const original = storeDown()
    const cloned = { json: async () => ({ code: 'auth_store_unavailable' }) }
    original.clone = vi.fn(() => cloned)
    const { win } = harness([original])
    const res = await win.fetch('/api/v1/findings')
    expect(original.clone).toHaveBeenCalledOnce()
    expect(res).toBe(original)
  })

  it('restores once after degraded, on the next successful API call', async () => {
    const { win, seen } = harness([storeDown(), storeDown(), ok(), ok()])
    await win.fetch('/api/v1/findings')
    await win.fetch('/api/v1/findings')
    expect(seen.restored).not.toHaveBeenCalled()
    await win.fetch('/api/v1/findings')
    await win.fetch('/api/v1/findings')
    expect(seen.degraded).toHaveBeenCalledTimes(2)
    expect(seen.restored).toHaveBeenCalledTimes(1)
  })

  it('does not fire restored for successes when never degraded', async () => {
    const { win, seen } = harness([ok()])
    await win.fetch('/api/v1/findings')
    expect(seen.restored).not.toHaveBeenCalled()
  })

  it('reports a degraded /auth/me too, but never expires on it', async () => {
    const { win, seen } = harness([storeDown(), { ok: false, status: 401 }])
    await win.fetch('/api/v1/auth/me')
    await win.fetch('/api/v1/auth/me')
    expect(seen.degraded).toHaveBeenCalledTimes(1)
    expect(seen.expired).not.toHaveBeenCalled()
  })

  it('ignores non-API URLs', async () => {
    const { win, seen } = harness([storeDown(), { ok: false, status: 401 }])
    await win.fetch('/assets/app.js')
    await win.fetch('https://example.com/api/v1x')
    expect(seen.degraded).not.toHaveBeenCalled()
    expect(seen.expired).not.toHaveBeenCalled()
  })
})
