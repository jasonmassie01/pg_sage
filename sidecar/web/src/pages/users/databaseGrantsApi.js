// apiauth-08 per-database grant calls for the Users page. Each throws an
// Error carrying the server's message on failure.

async function readJSON(res, fallback) {
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || fallback)
  return data
}

function grantsURL(userID, database) {
  const base = `/api/v1/users/${userID}/database-grants`
  return database === undefined ? base : `${base}/${encodeURIComponent(database)}`
}

export async function listDatabaseGrants(userID) {
  const res = await fetch(grantsURL(userID), { credentials: 'include' })
  return readJSON(res, 'Failed to load database grants')
}

export async function setDatabaseGrant(userID, database, role) {
  const res = await fetch(grantsURL(userID, database), {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'include',
    body: JSON.stringify({ role }),
  })
  return readJSON(res, 'Failed to save the grant')
}

export async function revokeDatabaseGrant(userID, database) {
  const res = await fetch(grantsURL(userID, database), {
    method: 'DELETE',
    credentials: 'include',
  })
  return readJSON(res, 'Failed to remove the grant')
}
