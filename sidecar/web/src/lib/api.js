// Shared API client. The server answers a failure with {"error": msg}
// (internal/api/helpers.go jsonError); these helpers put that message in
// front of the operator instead of a bare status code, which under HTTP/2
// carries no reason phrase at all.
//
// A 401 still reaches lib/authExpiry.js first: its fetch interceptor
// dispatches sage:auth-expired, and here it only becomes "session expired".

// ApiError is a failed API response: message is what the operator reads,
// status the HTTP status, serverMessage the server's own error text ('' if
// the body carried none).
export class ApiError extends Error {
  constructor(message, status, serverMessage = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.serverMessage = serverMessage
  }
}

// parseError reads a failed response's body once and returns an ApiError:
// "msg (404)" for a JSON {error}, else "Request failed (502 Bad Gateway)",
// or "Request failed (500)" when there is no reason phrase.
export async function parseError(res) {
  const serverMessage = serverMessageOf(await readText(res))
  if (serverMessage) {
    return new ApiError(`${serverMessage} (${res.status})`, res.status, serverMessage)
  }
  const line = res.statusText ? `${res.status} ${res.statusText}` : `${res.status}`
  return new ApiError(`Request failed (${line})`, res.status, '')
}

// apiGet fetches url and returns its JSON, or throws an ApiError. An abort
// rejects with the browser's AbortError, unchanged.
export async function apiGet(url, { signal } = {}) {
  const res = await fetch(url, { signal })
  await throwIfFailed(res)
  return res.json()
}

// apiSend sends body (when given) as JSON with method and returns the JSON
// reply, or null for an empty one; a failure throws an ApiError.
export async function apiSend(method, url, body) {
  const init = { method }
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }
  const res = await fetch(url, init)
  await throwIfFailed(res)
  const text = await res.text()
  return text ? JSON.parse(text) : null
}

async function throwIfFailed(res) {
  if (res.status === 401) throw new ApiError('session expired', 401, '')
  if (!res.ok) throw await parseError(res)
}

// readText is the body as text, or '' when it was already read (the
// browser's TypeError). An abort while reading still rejects.
async function readText(res) {
  try {
    return await res.text()
  } catch (err) {
    if (err instanceof TypeError) return ''
    throw err
  }
}

function serverMessageOf(text) {
  if (!text) return ''
  let body
  try {
    body = JSON.parse(text)
  } catch (err) {
    if (err instanceof SyntaxError) return ''
    throw err
  }
  const msg = body && typeof body === 'object' ? body.error : null
  return typeof msg === 'string' ? msg.trim() : ''
}
