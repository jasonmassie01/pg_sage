import { vi } from 'vitest'

// stubFetch installs a fetch double built on real Response objects, so
// code under test sees real status, ok, statusText and body parsing.
//
// routes maps a key to a reply. The key is "METHOD /path?query",
// "/path?query", "METHOD /path" or "/path"; the first key that matches
// wins in that order. A reply is:
//   - a spec: { status = 200, statusText = '', json, text, headers }
//     (json is encoded; text is sent as is; neither means an empty body)
//   - a function (url, init, callIndex) returning a spec or a Promise
//     of one, for deferred or call-dependent replies
//   - an array of replies, used one per call; the last one repeats.
// Like the browser, an aborted request rejects with an AbortError even
// while its reply is still pending. A request with no route rejects
// with an error naming it. The returned vi.fn records every call.
export function stubFetch(routes = {}) {
  const counts = new Map()
  const fn = vi.fn((input, init = {}) => {
    const url = typeof input === 'string' ? input : input.url
    const method = (init.method || 'GET').toUpperCase()
    const key = matchRoute(routes, method, url)
    if (key === null) {
      return Promise.reject(new Error(`stubFetch: no route for ${method} ${url}`))
    }
    const n = counts.get(key) || 0
    counts.set(key, n + 1)
    const reply = pickReply(routes[key], n)
    const pending = Promise.resolve(
      typeof reply === 'function' ? reply(url, init, n) : reply,
    ).then(toResponse)
    return withAbort(pending, init.signal)
  })
  vi.stubGlobal('fetch', fn)
  return fn
}

function matchRoute(routes, method, url) {
  const path = url.split('?')[0]
  const keys = [`${method} ${url}`, url, `${method} ${path}`, path]
  return keys.find(k => Object.hasOwn(routes, k)) ?? null
}

function pickReply(reply, n) {
  if (!Array.isArray(reply)) return reply
  if (reply.length === 0) throw new Error('stubFetch: empty reply list')
  return reply[Math.min(n, reply.length - 1)]
}

function toResponse(spec = {}) {
  const { status = 200, statusText = '', headers = {} } = spec
  let body = null
  const hdrs = new Headers(headers)
  if (spec.json !== undefined) {
    body = JSON.stringify(spec.json)
    if (!hdrs.has('content-type')) hdrs.set('content-type', 'application/json')
  } else if (spec.text !== undefined) {
    body = String(spec.text)
  }
  return new Response(body, { status, statusText, headers: hdrs })
}

function withAbort(pending, signal) {
  if (!signal) return pending
  const aborted = () => new DOMException('The operation was aborted.', 'AbortError')
  if (signal.aborted) return Promise.reject(aborted())
  return new Promise((resolve, reject) => {
    const onAbort = () => reject(aborted())
    signal.addEventListener('abort', onAbort, { once: true })
    pending.then(resolve, reject).finally(() => signal.removeEventListener('abort', onAbort))
  })
}
