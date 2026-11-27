// apiauth-03 password calls. Each throws an Error carrying the server's
// message on failure; the passwords are never logged or stored.

async function readJSON(res, fallback) {
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || fallback)
  return data
}

export async function changePassword(current, next) {
  const res = await fetch('/api/v1/auth/password', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: JSON.stringify({ current_password: current, new_password: next }),
  })
  return readJSON(res, 'Failed to change password')
}

export async function resetUserPassword(id) {
  const res = await fetch(`/api/v1/users/${id}/password-reset`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: '{}',
  })
  return readJSON(res, 'Failed to reset password')
}

// passwordProblem mirrors the server policy (12 to 72 bytes, not the
// account email) so the form can answer before a round trip. The server
// still decides.
export function passwordProblem(email, next, confirm) {
  const bytes = new TextEncoder().encode(next).length
  if (bytes < 12) return 'Use at least 12 bytes.'
  if (bytes > 72) return 'Use at most 72 bytes.'
  if (email && next.trim().toLowerCase() === email.trim().toLowerCase()) {
    return 'The password must not be your email address.'
  }
  if (next !== confirm) return 'The new passwords do not match.'
  return null
}
