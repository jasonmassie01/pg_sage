// connectionUrl parses a pasted PostgreSQL connection URL in the browser
// into the DatabaseForm fields (ux-04b). Nothing is sent anywhere; the
// password never appears in an error or in the masked URL.

export const SSL_MODES = ['disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full']

const SCHEMES = new Set(['postgres', 'postgresql'])
const URL_RE = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)([^?#]*)(\?[^#]*)?(#.*)?$/
const HOST_RE = /^(?:\[([^\]]*)\]|([^:]*))(?::(.*))?$/
const PASTE_HINT = 'Paste a connection URL (postgres://user:password@host:5432/database)'

function decode(part) {
  return decodeURIComponent(part)
}

// splitAuthority separates user, password, host and port text.
function splitAuthority(authority) {
  const at = authority.lastIndexOf('@')
  const userinfo = at >= 0 ? authority.slice(0, at) : ''
  const colon = userinfo.indexOf(':')
  const user = colon >= 0 ? userinfo.slice(0, colon) : userinfo
  const password = colon >= 0 ? userinfo.slice(colon + 1) : ''
  const m = HOST_RE.exec(authority.slice(at + 1))
  return { user, password, host: m?.[1] ?? m?.[2] ?? '', port: m?.[3] }
}

function readPort(text) {
  if (text === undefined || text === '') return { port: 5432 }
  const port = /^\d+$/.test(text) ? Number(text) : NaN
  if (!(port >= 1 && port <= 65535)) return { error: 'The port must be a number from 1 to 65535' }
  return { port }
}

function readSSLMode(query) {
  const params = new URLSearchParams(query || '')
  const mode = params.get('sslmode')
  if (mode === null || mode === '') return { sslmode: 'require' }
  if (!SSL_MODES.includes(mode)) {
    return { error: `Unknown sslmode "${mode}" (want ${SSL_MODES.join(', ')})` }
  }
  return { sslmode: mode }
}

// safeName is a pg_sage database name: letters, digits, _ - and . only.
function safeName(text) {
  return text.replace(/[^A-Za-z0-9_.-]/g, '-').slice(0, 63)
}

function schemeError(text) {
  const m = /^([A-Za-z][A-Za-z0-9+.-]*):/.exec(text)
  if (m && !SCHEMES.has(m[1].toLowerCase()) && text.slice(m[0].length).startsWith('//')) {
    return `Unknown scheme "${m[1]}:" (want postgres:// or postgresql://)`
  }
  return 'Unknown scheme: the URL must start with postgres:// or postgresql://'
}

function readParts(m) {
  const parts = splitAuthority(m[2])
  if (parts.host === '') return { error: 'The URL has no host' }
  const port = readPort(parts.port)
  if (port.error) return port
  if (parts.user === '') return { error: 'The URL has no user' }
  const ssl = readSSLMode(m[4]?.slice(1))
  if (ssl.error) return ssl
  try {
    const username = decode(parts.user)
    const database = decode(m[3].replace(/^\//, '')) || username
    return { fields: { name: safeName(database), host: parts.host, port: port.port,
      database_name: database, username, password: decode(parts.password),
      sslmode: ssl.sslmode } }
  } catch (err) {
    if (err instanceof URIError) return { error: 'The URL has a bad percent escape (%)' }
    throw err
  }
}

// parseConnectionURL returns { fields } for a usable URL, else { error }.
// Defaults follow libpq: port 5432 and the user's name as the database;
// sslmode defaults to require, as DatabaseForm does.
export function parseConnectionURL(text) {
  const trimmed = String(text ?? '').trim()
  if (trimmed === '') return { error: PASTE_HINT }
  const m = URL_RE.exec(trimmed)
  if (!m || !SCHEMES.has(m[1].toLowerCase())) return { error: schemeError(trimmed) }
  return readParts(m)
}

// maskConnectionURL replaces the password with ****; text that is not a
// URL is dropped entirely, since it may be a password.
export function maskConnectionURL(text) {
  const trimmed = String(text ?? '').trim()
  const m = /^([A-Za-z][A-Za-z0-9+.-]*:\/\/)([^/?#]*)(.*)$/.exec(trimmed)
  if (!m) return ''
  const at = m[2].lastIndexOf('@')
  if (at < 0) return trimmed
  const userinfo = m[2].slice(0, at)
  const colon = userinfo.indexOf(':')
  if (colon < 0) return trimmed
  return `${m[1]}${userinfo.slice(0, colon)}:****${m[2].slice(at)}${m[3]}`
}
