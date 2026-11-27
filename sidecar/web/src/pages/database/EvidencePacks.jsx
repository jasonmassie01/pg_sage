import { useCallback, useEffect, useState } from 'react'
import { apiGet, apiSend } from '../../lib/api'
import { ModelTierView } from './ModelTierView'

// EvidencePacks is the database page's workload evidence pack card
// (evid-02a..c): an operator builds a pack of the last 28 days; anyone who
// can read the database lists the packs and opens "What would leave", the
// model tier a model would read; an admin downloads the signed export,
// which holds the local tier's statement texts. The API enforces the same
// roles.

const packsURL = db => `/api/v1/databases/${encodeURIComponent(db)}/workload-packs`

function roleOf(user) {
  return typeof user?.role === 'string' ? user.role : ''
}

function windowText(w) {
  if (!w?.from || !w?.to) return 'unknown window'
  return `${String(w.from).slice(0, 10)} to ${String(w.to).slice(0, 10)}`
}

function usePacks(database) {
  const [packs, setPacks] = useState(null)
  const [error, setError] = useState(null)
  const [version, setVersion] = useState(0)
  useEffect(() => {
    let cancelled = false
    apiGet(packsURL(database))
      .then(body => {
        if (cancelled) return
        setPacks(Array.isArray(body?.packs) ? body.packs : [])
        setError(null)
      })
      .catch(err => { if (!cancelled) setError(err.message) })
    return () => { cancelled = true }
  }, [database, version])
  const reload = useCallback(() => setVersion(v => v + 1), [])
  return { packs, error, reload }
}

function BuildButton({ database, onBuilt }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const build = async () => {
    setBusy(true)
    setError(null)
    try {
      await apiSend('POST', packsURL(database), {})
      onBuilt()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <span className="flex items-center gap-2">
      <button type="button" onClick={build} disabled={busy} data-testid="pack-build"
        className="px-3 py-1.5 rounded text-sm"
        style={{ background: 'var(--accent-solid)', color: '#fff', opacity: busy ? 0.6 : 1 }}>
        {busy ? 'Building...' : 'Build pack (last 28 days)'}
      </button>
      {error && (
        <span className="text-xs" data-testid="pack-build-error" style={{ color: 'var(--red)' }}>
          Build failed: {error}
        </span>
      )}
    </span>
  )
}

function PackRow({ database, pack, canExport, open, onToggle }) {
  const sha = pack.content_sha256 || ''
  return (
    <li className="py-2" data-testid={`pack-${sha.slice(0, 12)}`}>
      <div className="flex flex-wrap items-center gap-3 text-sm">
        <span className="font-mono" style={{ color: 'var(--text-primary)' }}>
          {sha.slice(0, 12)}
        </span>
        <span style={{ color: 'var(--text-secondary)' }}>{windowText(pack.window)}</span>
        <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          built {String(pack.built_at || '').slice(0, 16).replace('T', ' ')} by{' '}
          {pack.built_by || 'unknown'}
        </span>
        <button type="button" onClick={onToggle} data-testid={`pack-preview-${sha.slice(0, 12)}`}
          className="px-2 py-1 rounded text-xs"
          style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
          {open ? 'Hide' : 'What would leave'}
        </button>
        {canExport && (
          <a href={`${packsURL(database)}/${sha}/export`}
            download={`pg_sage-workload-${database}-${sha.slice(0, 12)}.tar.gz`}
            data-testid={`pack-export-${sha.slice(0, 12)}`}
            className="px-2 py-1 rounded text-xs"
            style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
            Download signed export
          </a>
        )}
      </div>
      {open && <ModelTier database={database} sha={sha} />}
    </li>
  )
}

function ModelTier({ database, sha }) {
  const [view, setView] = useState(null)
  const [error, setError] = useState(null)
  useEffect(() => {
    let cancelled = false
    apiGet(`${packsURL(database)}/${sha}?tier=model`)
      .then(body => { if (!cancelled) setView(body) })
      .catch(err => { if (!cancelled) setError(err.message) })
    return () => { cancelled = true }
  }, [database, sha])
  if (error) {
    return (
      <p className="text-xs mt-2" data-testid="model-tier-error" style={{ color: 'var(--red)' }}>
        The model tier is not available: {error}
      </p>
    )
  }
  if (!view) {
    return <p className="text-xs mt-2" style={{ color: 'var(--text-secondary)' }}>Loading...</p>
  }
  return <ModelTierView view={view} />
}

export function EvidencePacks({ database, user }) {
  const { packs, error, reload } = usePacks(database)
  const [open, setOpen] = useState(null)
  const role = roleOf(user)
  const canBuild = role === 'operator' || role === 'admin'
  return (
    <section className="rounded p-4" data-testid="evidence-packs"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <div className="flex flex-wrap items-center justify-between gap-2 mb-2">
        <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
          Evidence packs
        </h2>
        {canBuild && <BuildButton database={database} onBuilt={reload} />}
      </div>
      <p className="text-xs mb-2" style={{ color: 'var(--text-secondary)' }}>
        A pack is this database&apos;s workload over a window, hashed and stored.
        &quot;What would leave&quot; shows the redacted model tier before any model reads it.
      </p>
      {error && (
        <p className="text-sm" data-testid="packs-error" style={{ color: 'var(--red)' }}>
          Could not list packs: {error}
        </p>
      )}
      {!error && packs && packs.length === 0 && (
        <p className="text-sm" data-testid="packs-empty" style={{ color: 'var(--text-secondary)' }}>
          No evidence pack yet.
        </p>
      )}
      {packs && packs.length > 0 && (
        <ul className="divide-y" style={{ borderColor: 'var(--border)' }}>
          {packs.map(p => (
            <PackRow key={p.content_sha256} database={database} pack={p}
              canExport={role === 'admin'} open={open === p.content_sha256}
              onToggle={() => setOpen(open === p.content_sha256 ? null : p.content_sha256)} />
          ))}
        </ul>
      )}
    </section>
  )
}
