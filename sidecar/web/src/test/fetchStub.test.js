import { afterEach, describe, expect, it, vi } from 'vitest'
import { stubFetch } from './fetchStub'

afterEach(() => vi.unstubAllGlobals())

// The stub is shared test infrastructure: a wrong status or body here
// would make every hook test that uses it lie.
describe('stubFetch', () => {
  it('replies with a real JSON Response', async () => {
    stubFetch({ '/a': { json: { ok: 1 } } })
    const res = await fetch('/a')
    expect(res).toBeInstanceOf(Response)
    expect(res.status).toBe(200)
    expect(res.ok).toBe(true)
    expect(res.headers.get('content-type')).toBe('application/json')
    expect(await res.json()).toEqual({ ok: 1 })
  })

  it('replies with text, an empty body and an error status', async () => {
    stubFetch({
      '/t': { text: 'plain' },
      '/e': { status: 204 },
      '/x': { status: 500, statusText: 'Internal Server Error' },
    })
    expect(await (await fetch('/t')).text()).toBe('plain')
    const empty = await fetch('/e')
    expect(empty.status).toBe(204)
    expect(await empty.text()).toBe('')
    const bad = await fetch('/x')
    expect(bad.ok).toBe(false)
    expect(bad.statusText).toBe('Internal Server Error')
  })

  it('prefers method and query keys over the bare path', async () => {
    stubFetch({
      'POST /a': { json: 'post' },
      '/a?x=1': { json: 'query' },
      '/a': { json: 'path' },
    })
    expect(await (await fetch('/a', { method: 'post' })).json()).toBe('post')
    expect(await (await fetch('/a?x=1')).json()).toBe('query')
    expect(await (await fetch('/a?x=2')).json()).toBe('path')
  })

  it('uses a reply list in order and repeats its last entry', async () => {
    stubFetch({ '/n': [{ json: 1 }, (url, init, n) => ({ json: `${url}#${n}` })] })
    const got = []
    for (let i = 0; i < 3; i++) got.push(await (await fetch('/n')).json())
    expect(got).toEqual([1, '/n#1', '/n#2'])
  })

  it('rejects an unrouted request with its method and url', async () => {
    stubFetch({})
    await expect(fetch('/missing', { method: 'DELETE' }))
      .rejects.toThrow('stubFetch: no route for DELETE /missing')
  })

  it('rejects an empty reply list', async () => {
    stubFetch({ '/a': [] })
    expect(() => fetch('/a')).toThrow('stubFetch: empty reply list')
  })

  it('rejects a pending request with AbortError when aborted', async () => {
    stubFetch({ '/slow': () => new Promise(() => {}) })
    const ctrl = new AbortController()
    const pending = fetch('/slow', { signal: ctrl.signal })
    ctrl.abort()
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' })
    await expect(fetch('/slow', { signal: ctrl.signal }))
      .rejects.toMatchObject({ name: 'AbortError' })
  })

  it('records calls on the returned mock', async () => {
    const fn = stubFetch({ '/a': { json: null } })
    await fetch('/a', { method: 'GET' })
    expect(fn).toHaveBeenCalledWith('/a', { method: 'GET' })
  })
})
