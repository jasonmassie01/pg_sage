// apiauth-01 user administration calls for the Users page. Each throws an
// Error carrying the server's message on failure.

async function readJSON(res, fallback) {
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || fallback)
  return data
}

export async function setUserDisabled(id, disabled) {
  const res = await fetch(`/api/v1/users/${id}/status`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: JSON.stringify({ disabled }),
  })
  return readJSON(res, disabled ? 'Failed to deactivate user' : 'Failed to reactivate user')
}

export async function deleteUser(id) {
  const res = await fetch(`/api/v1/users/${id}`, {
    method: 'DELETE',
    credentials: 'include',
  })
  return readJSON(res, 'Failed to delete user')
}
