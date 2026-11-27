import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiGet, apiSend, parseError } from './api'
import { stubFetch } from '../test/fetchStub'

afterEach(() => vi.unstubAllGlobals())

// dashboard-05: the server answers failures with {"error": msg}; the
// operator must see that message, not a bare status code.

// No concurrent access tests: these helpers are stateless and share no
// references between calls.

function response(body, init) {
  return new Response(body, init)
}

describe('parseError', () => {
  it('uses the server message from a JSON error body', async () => {
    const res = response(JSON.stringify({ error: 'database not found' }),
      { status: 404, statusText: 'Not Found' })
    const err = await parseError(res)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toBeInstanceOf(Error)
    expect(err.name).toBe('ApiError')
    expect(err.message).toBe('database not found (404)')
    expect(err.status).toBe(404)
    expect(err.serverMessage).toBe('database not found')
  })

  it('trims the server message', async () => {
    const res = response('{"error":"  lock timeout  "}', { status: 409 })
    const err = await parseError(res)
    expect(err.message).toBe('lock timeout (409)')
    expect(err.serverMessage).toBe('lock timeout')
  })

  it('falls back to the status line for an HTML body without throwing', async () => {
    const res = response('<html><body>Bad Gateway</body></html>',
      { status: 502, statusText: 'Bad Gateway' })
    const err = await parseError(res)
    expect(err.message).toBe('Request failed (502 Bad Gateway)')
    expect(err.status).toBe(502)
    expect(err.serverMessage).toBe('')
  })

  it('gives the bare status for an empty body and empty statusText', async () => {
    const err = await parseError(response(null, { status: 500 }))
    expect(err.message).toBe('Request failed (500)')
    expect(err.status).toBe(500)
    expect(err.serverMessage).toBe('')
  })

  it('falls back to the status line when the error field is empty', async () => {
    const res = response('{"error":""}', { status: 503, statusText: 'Service Unavailable' })
    const err = await parseError(res)
    expect(err.message).toBe('Request failed (503 Service Unavailable)')
    expect(err.serverMessage).toBe('')
  })

  it('ignores an error field that is not a string', async () => {
    const res = response('{"error":{"code":7}}', { status: 400 })
    expect((await parseError(res)).message).toBe('Request failed (400)')
  })

  it('ignores JSON that is not an object with an error', async () => {
    expect((await parseError(response('null', { status: 500 }))).message)
      .toBe('Request failed (500)')
    expect((await parseError(response('"oops"', { status: 500 }))).message)
      .toBe('Request failed (500)')
    expect((await parseError(response('{"message":"x"}', { status: 500 }))).message)
      .toBe('Request failed (500)')
  })

  it('reads the body only once', async () => {
    const res = response('{"error":"once"}', { status: 400 })
    const text = vi.spyOn(res, 'text')
    const json = vi.spyOn(res, 'json')
    await parseError(res)
    expect(text.mock.calls.length + json.mock.calls.length).toBe(1)
  })

  it('treats a body that was already read as empty', async () => {
    const res = response('{"error":"gone"}', { status: 400, statusText: 'Bad Request' })
    await res.text()
    expect((await parseError(res)).message).toBe('Request failed (400 Bad Request)')
  })

  it('propagates an abort while the body is read', async () => {
    const res = { status: 500, statusText: '', text: () =>
      Promise.reject(new DOMException('The operation was aborted.', 'AbortError')) }
    await expect(parseError(res)).rejects.toMatchObject({ name: 'AbortError' })
  })
})

describe('apiGet', () => {
  it('returns the parsed JSON and passes the signal', async () => {
    const fetch = stubFetch({ '/api/v1/findings': { json: { findings: [1] } } })
    const ctrl = new AbortController()
    await expect(apiGet('/api/v1/findings', { signal: ctrl.signal }))
      .resolves.toEqual({ findings: [1] })
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/findings')
    expect(fetch.mock.calls[0][1].signal).toBe(ctrl.signal)
  })

  it('works without options', async () => {
    stubFetch({ '/api/v1/health': { json: { ok: true } } })
    await expect(apiGet('/api/v1/health')).resolves.toEqual({ ok: true })
  })

  it('throws an ApiError carrying the server message and status', async () => {
    stubFetch({ '/api/v1/databases/x': { status: 404, json: { error: 'database not found' } } })
    const err = await apiGet('/api/v1/databases/x').catch(e => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.message).toBe('database not found (404)')
    expect(err.status).toBe(404)
  })

  it('reports an expired session on 401', async () => {
    stubFetch({ '/api/v1/actions': { status: 401, json: { error: 'unauthorized' } } })
    const err = await apiGet('/api/v1/actions').catch(e => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.message).toBe('session expired')
    expect(err.status).toBe(401)
  })

  it('rejects a 200 body that is not JSON', async () => {
    stubFetch({ '/api/v1/actions': { text: '<html>proxy</html>' } })
    await expect(apiGet('/api/v1/actions')).rejects.toThrow(/JSON/)
  })

  it('lets an abort reject with AbortError', async () => {
    stubFetch({ '/api/v1/actions': () => new Promise(() => {}) })
    const ctrl = new AbortController()
    const pending = apiGet('/api/v1/actions', { signal: ctrl.signal })
    ctrl.abort()
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' })
  })

  it('lets a network failure through unchanged', async () => {
    stubFetch({ '/api/v1/actions': () => Promise.reject(new TypeError('Failed to fetch')) })
    const err = await apiGet('/api/v1/actions').catch(e => e)
    expect(err).toBeInstanceOf(TypeError)
    expect(err.message).toBe('Failed to fetch')
  })
})

describe('apiSend', () => {
  it('sends a JSON body with its content type and returns the JSON reply', async () => {
    const fetch = stubFetch({ 'POST /api/v1/actions/7/approve': { json: { id: 7 } } })
    await expect(apiSend('POST', '/api/v1/actions/7/approve', { a: 1 }))
      .resolves.toEqual({ id: 7 })
    const [url, init] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/actions/7/approve')
    expect(init.method).toBe('POST')
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' })
    expect(init.body).toBe('{"a":1}')
  })

  it('sends no body and no content type without a body', async () => {
    const fetch = stubFetch({ 'DELETE /api/v1/rules/3': { status: 204 } })
    await expect(apiSend('DELETE', '/api/v1/rules/3')).resolves.toBeNull()
    const init = fetch.mock.calls[0][1]
    expect(init.method).toBe('DELETE')
    expect(init.body).toBeUndefined()
    expect(init.headers).toBeUndefined()
  })

  it('returns null for an empty 200 reply', async () => {
    stubFetch({ 'PUT /api/v1/config': { status: 200 } })
    await expect(apiSend('PUT', '/api/v1/config', { x: 0 })).resolves.toBeNull()
  })

  it('throws the server message on failure', async () => {
    stubFetch({ 'POST /api/v1/actions/7/rollback':
      { status: 409, json: { error: 'action already rolled back' } } })
    const err = await apiSend('POST', '/api/v1/actions/7/rollback', {}).catch(e => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.message).toBe('action already rolled back (409)')
    expect(err.status).toBe(409)
  })

  it('reports an expired session on 401', async () => {
    stubFetch({ 'POST /api/v1/x': { status: 401 } })
    const err = await apiSend('POST', '/api/v1/x', {}).catch(e => e)
    expect(err.message).toBe('session expired')
    expect(err.status).toBe(401)
  })
})
