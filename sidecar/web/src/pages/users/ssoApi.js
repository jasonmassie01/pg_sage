// D7 account-linking API calls shared by the Users, Profile and Link SSO
// pages. Each throws an Error carrying the server's message on failure.

async function readJSON(res, fallback) {
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || fallback)
  return data
}

export async function startSSOLink() {
  const res = await fetch('/api/v1/auth/oauth/authorize?intent=link', {
    credentials: 'include',
  })
  const data = await readJSON(res, 'Could not start SSO linking')
  return data.url
}

export async function redeemSSOLinkGrant(grant) {
  const res = await fetch('/api/v1/auth/oauth/link-grant', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ grant }),
  })
  const data = await readJSON(res, 'Could not use this SSO link')
  return data.url
}

export async function fetchSSOStatus() {
  const res = await fetch('/api/v1/auth/sso', { credentials: 'include' })
  return readJSON(res, 'Could not load SSO status')
}

export async function unlinkUserSSO(id) {
  const res = await fetch(`/api/v1/users/${id}/oidc`, {
    method: 'DELETE',
    credentials: 'include',
  })
  return readJSON(res, 'Failed to unlink SSO')
}

export async function issueSSOLinkGrant(id) {
  const res = await fetch(`/api/v1/users/${id}/oidc-link-grant`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: '{}',
  })
  return readJSON(res, 'Failed to issue SSO link')
}

export function grantLinkURL(token) {
  return `${window.location.origin}/#/link-sso?grant=${encodeURIComponent(token)}`
}

export function hashParam(name) {
  const hash = window.location.hash || ''
  const query = hash.includes('?') ? hash.slice(hash.indexOf('?') + 1) : ''
  return new URLSearchParams(query).get(name) || ''
}

export function redirectTo(url) {
  window.location.assign(url)
}
