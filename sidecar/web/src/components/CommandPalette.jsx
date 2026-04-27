import { useEffect, useRef, useState } from 'react'
import { Search } from 'lucide-react'

const NAV_COMMANDS = [
  { id: 'nav-dashboard', label: 'Go to Overview',
    hint: 'Agent state + fleet health', hash: '#/' },
  { id: 'nav-cases', label: 'Go to Cases',
    hint: 'Open DBA work queue', hash: '#/cases' },
  { id: 'nav-actions', label: 'Go to Actions',
    hint: 'Executed + pending timeline', hash: '#/actions' },
  { id: 'nav-databases', label: 'Go to Fleet',
    hint: 'Manage database connections', hash: '#/manage-databases',
    admin: true },
  { id: 'nav-settings', label: 'Go to Settings',
    hint: 'Configure pg_sage', hash: '#/settings', admin: true },
]

function score(query, text) {
  if (!query) return 1
  const q = query.toLowerCase()
  const t = text.toLowerCase()
  if (t.includes(q)) return 2 + (t.startsWith(q) ? 1 : 0)
  let qi = 0
  for (let i = 0; i < t.length && qi < q.length; i++) {
    if (t[i] === q[qi]) qi++
  }
  return qi === q.length ? 1 : 0
}

export function CommandPalette({
  databases = [], selectedDB, onSelectDB, user,
}) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [index, setIndex] = useState(0)
  const inputRef = useRef(null)

  const openPalette = (initialQuery = '') => {
    setQuery(initialQuery)
    setIndex(0)
    setOpen(true)
  }

  useEffect(() => {
    const onKey = e => {
      const mod = e.metaKey || e.ctrlKey
      if (mod && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        if (open) setOpen(false)
        else openPalette()
      } else if (e.key === 'Escape' && open) {
        setOpen(false)
      } else if (e.key === '?' && !open
        && !isTyping(e.target)) {
        e.preventDefault()
        openPalette('?')
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open])

  useEffect(() => {
    if (open) {
      requestAnimationFrame(() => inputRef.current?.focus())
    }
  }, [open])

  if (!open) return null

  const dbCommands = (databases || []).map(d => ({
    id: `db-${d.name}`,
    label: `Select database: ${d.name}`,
    hint: selectedDB === d.name ? 'current' : 'switch',
    run: () => onSelectDB?.(d.name),
  }))
  const allDbCmd = {
    id: 'db-all',
    label: 'Select all databases',
    hint: selectedDB === 'all' ? 'current' : 'overview',
    run: () => onSelectDB?.('all'),
  }
  const isAdmin = user?.role === 'admin'
  const shortcut = navigator.platform?.toLowerCase().includes('mac')
    ? '⌘K' : 'Ctrl+K'
  const navCommands = NAV_COMMANDS
    .filter(n => !n.admin || isAdmin)
    .map(n => ({
    ...n,
    run: () => { window.location.hash = n.hash },
  }))
  const helpCmd = {
    id: 'help',
    label: 'Keyboard shortcuts',
    hint: `${shortcut} command palette, ? help`,
    run: () => {},
  }
  const pool = [...navCommands, allDbCmd, ...dbCommands, helpCmd]

  const ranked = pool
    .map(c => ({ c, s: score(query, c.label + ' ' + c.hint) }))
    .filter(x => x.s > 0)
    .sort((a, b) => b.s - a.s)
    .map(x => x.c)

  const pick = i => {
    const cmd = ranked[i]
    if (!cmd) return
    cmd.run()
    setOpen(false)
  }

  const onKeyDown = e => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setIndex(i => Math.min(i + 1, ranked.length - 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setIndex(i => Math.max(i - 1, 0))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      pick(index)
    }
  }

  return (
    <div
      data-testid="command-palette"
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
      onClick={() => setOpen(false)}
      style={{
        position: 'fixed', inset: 0, zIndex: 50,
        background: 'rgba(0,0,0,0.5)',
        display: 'flex', justifyContent: 'center',
        paddingTop: '12vh',
      }}>
      <div
        onClick={e => e.stopPropagation()}
        className="rounded"
        style={{
          width: '100%', maxWidth: 560,
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
          boxShadow: '0 10px 40px rgba(0,0,0,0.5)',
          maxHeight: '70vh', display: 'flex',
          flexDirection: 'column',
        }}>
        <div className="flex items-center gap-2 p-3"
          style={{ borderBottom: '1px solid var(--border)' }}>
          <Search size={16}
            style={{ color: 'var(--text-secondary)' }} />
          <input
            ref={inputRef}
            data-testid="command-palette-input"
            value={query}
            onChange={e => { setQuery(e.target.value); setIndex(0) }}
            onKeyDown={onKeyDown}
            placeholder="Type a command or search..."
            className="flex-1 bg-transparent outline-none text-sm"
            style={{ color: 'var(--text-primary)' }}
          />
          <span className="text-xs px-1.5 py-0.5 rounded"
            style={{
              background: 'var(--bg-hover)',
              color: 'var(--text-secondary)',
            }}>Esc</span>
        </div>
        <div className="overflow-auto" style={{ flex: 1 }}>
          {ranked.length === 0 && (
            <div className="p-4 text-sm text-center"
              style={{ color: 'var(--text-secondary)' }}>
              No matches
            </div>
          )}
          {ranked.map((c, i) => (
            <button
              key={c.id}
              type="button"
              data-testid={`command-${c.id}`}
              onMouseEnter={() => setIndex(i)}
              onClick={() => pick(i)}
              className="w-full flex items-center justify-between px-3 py-2 text-sm text-left"
              style={{
                background: i === index
                  ? 'var(--bg-hover)' : 'transparent',
                color: 'var(--text-primary)',
              }}>
              <span>{c.label}</span>
              <span className="text-xs"
                style={{ color: 'var(--text-secondary)' }}>
                {c.hint}
              </span>
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}

function isTyping(el) {
  if (!el) return false
  const tag = (el.tagName || '').toLowerCase()
  return tag === 'input' || tag === 'textarea'
    || el.isContentEditable
}
