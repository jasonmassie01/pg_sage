// shell.ts: app-shell routes and browser doubles for the dashboard-16
// specs (status strip, freshness, session-store banner, storage blocked,
// hidden tab, session end). Every payload names the Go code whose response
// it copies. Register these routes AFTER mockAllAPIs: Playwright matches the
// newest first.

import { type Page, type Request } from '@playwright/test'

/* ---------- Responses ---------- */

// BAD_GATEWAY_HTML is what nginx answers while the sidecar behind it
// restarts: a 502 with an HTML body, not the API's {"error": msg} JSON.
export const BAD_GATEWAY_HTML = '<html><head><title>502 Bad Gateway</title></head>' +
  '<body><center><h1>502 Bad Gateway</h1></center><hr><center>nginx</center></body></html>'

// STORE_DOWN copies writeSessionStoreUnavailable
// (internal/api/session_store_errors.go): 503, Retry-After 5 and
// {"error": "session store unavailable", "code": "auth_store_unavailable"}.
export const STORE_DOWN = {
  status: 503,
  headers: { 'Retry-After': '5' },
  json: { error: 'session store unavailable', code: 'auth_store_unavailable' },
}

// NOT_AUTHENTICATED is the 401 a refused session gets (meHandler and the
// session middleware answer jsonError "not authenticated"), as a
// deactivated user's next request does (apiauth-01 ValidateSession).
export const NOT_AUTHENTICATED = { status: 401, json: { error: 'not authenticated' } }

// STREAM_BODY is a short /api/v1/events stream: internal/api/events.go
// greets with a retry hint and writes "event: <type>" plus a JSON data line
// per event. retry here is 1 s (the server sends 3 s), so the browser reopens
// the stream soon after the mocked body ends, as it does after the server's
// stream deadline.
export const STREAM_BODY = 'retry: 1000\n\nevent: heartbeat\ndata: {"type":"heartbeat"}\n\n'

/* ---------- Routes ---------- */

// routeEvents serves /api/v1/events as text/event-stream while up() is
// true, and the proxy's 502 otherwise.
export async function routeEvents(page: Page, up: () => boolean = () => true) {
  await page.route('**/api/v1/events**', route => {
    if (!up()) {
      return route.fulfill({ status: 502, contentType: 'text/html', body: BAD_GATEWAY_HTML })
    }
    return route.fulfill({
      status: 200,
      headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' },
      body: STREAM_BODY,
    })
  })
}

// Outage answers a /api/v1/* request, or returns null to let the older
// routes (mockAllAPIs, routeEvents) answer it.
export type Outage = (request: Request) =>
  | { status: number, contentType?: string, body?: string, json?: unknown,
      headers?: Record<string, string> }
  | null

// routeOutage puts outage in front of every /api/v1/* route.
export async function routeOutage(page: Page, outage: Outage) {
  await page.route('**/api/v1/**', route => {
    const answer = outage(route.request())
    return answer ? route.fulfill(answer) : route.fallback()
  })
}

// proxy502 is the restart outage: every API path, HTML body.
export const proxy502 = { status: 502, contentType: 'text/html', body: BAD_GATEWAY_HTML }

/* ---------- Request log ---------- */

// apiPath is the request's path and query, or null for a non-API request.
export function apiPath(url: string): string | null {
  const u = new URL(url)
  return u.pathname.startsWith('/api/v1/') ? u.pathname + u.search : null
}

// isEvents is true for the live-events stream, which is not a poll.
export function isEvents(path: string) {
  return path.startsWith('/api/v1/events')
}

// logAPIRequests records every /api/v1/* request the page sends, in order.
export function logAPIRequests(page: Page): string[] {
  const log: string[] = []
  page.on('request', r => {
    const path = apiPath(r.url())
    if (path) log.push(path)
  })
  return log
}

/* ---------- Browser doubles ---------- */

// blockStorage makes the window.localStorage and window.sessionStorage
// accessors throw SecurityError, as Chrome does with site data blocked
// ("Block sites from saving data"): the throw comes from the getter itself,
// before any getItem call.
export async function blockStorage(page: Page) {
  await page.addInitScript(() => {
    for (const name of ['localStorage', 'sessionStorage']) {
      Object.defineProperty(window, name, {
        configurable: true,
        get() {
          throw new DOMException(
            `Failed to read the '${name}' property from 'Window': Access is denied ` +
            'for this document.', 'SecurityError')
        },
      })
    }
  })
}

// controlVisibility replaces document.visibilityState and document.hidden
// with a flag the test flips through window.__sageSetHidden(bool), which
// also dispatches visibilitychange as the browser does on a tab switch.
export async function controlVisibility(page: Page) {
  await page.addInitScript(() => {
    let hidden = false
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
    Object.defineProperty(document, 'visibilityState', {
      configurable: true, get: () => (hidden ? 'hidden' : 'visible'),
    })
    Object.assign(window, {
      __sageSetHidden(next: boolean) {
        hidden = next
        document.dispatchEvent(new Event('visibilitychange'))
      },
    })
  })
}

export async function setHidden(page: Page, hidden: boolean) {
  await page.evaluate(h => {
    (window as unknown as { __sageSetHidden: (v: boolean) => void }).__sageSetHidden(h)
  }, hidden)
}

// advanceUntil moves the installed fake clock in steps until check() is
// true: an answer to a request a timer sent arrives in real time, so one
// large jump cannot chain timer, response and the next timer.
export async function advanceUntil(page: Page, check: () => Promise<boolean>,
  { stepMs = 5_000, maxMs = 180_000 } = {}) {
  for (let spent = 0; spent <= maxMs; spent += stepMs) {
    if (await check()) return spent
    await page.clock.fastForward(stepMs)
    await page.waitForTimeout(100)
  }
  throw new Error(`condition not met after ${maxMs} ms of fake time`)
}
