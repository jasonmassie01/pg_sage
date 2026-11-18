import { useEffect, useRef, useState } from 'react'
import { Search } from 'lucide-react'
import { paletteRoutes, routeHref, routeLabel } from '../routes'
import { PALETTE_KEY, isTyping, shortcutFor } from './keymap'
import { ShortcutsSheet } from './ShortcutsSheet'

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

// buildCommands lists the routes the user can see (from the route
// registry), the database scopes and the shortcuts sheet.
function buildCommands({ databases, selectedDB, onSelectDB, user, authMode }) {
  const navCommands = paletteRoutes(user, authMode === 'databricks_app')
    .map(route => ({
      id: route.testId,
      label: `Go to ${routeLabel(route)}`,
      hint: route.hint,
      run: () => { window.location.hash = routeHref(route) },
    }))
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
  const helpCmd = {
    id: 'help',
    label: 'Keyboard shortcuts',
    hint: `${PALETTE_KEY} command palette, ? help`,
    opens: 'shortcuts',
  }
  return [...navCommands, allDbCmd, ...dbCommands, helpCmd]
}

// CommandPalette owns the global shortcuts: the palette (Ctrl/Cmd+K) and
// the shortcuts sheet (?). mode is null, 'palette' or 'shortcuts'.
export function CommandPalette({
  databases = [], selectedDB, onSelectDB, user, authMode = user?.auth_mode,
}) {
  const [mode, setMode] = useState(null)

  useEffect(() => {
    const onKey = e => {
      const shortcut = shortcutFor(e)
      if (shortcut === 'palette') {
        e.preventDefault()
        setMode(mode === 'palette' ? null : 'palette')
      } else if (shortcut === 'close' && mode) {
        setMode(null)
      } else if (shortcut === 'shortcuts' && !mode && !isTyping(e.target)) {
        e.preventDefault()
        setMode('shortcuts')
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [mode])

  if (mode === 'shortcuts') return <ShortcutsSheet onClose={() => setMode(null)} />
  if (mode !== 'palette') return null
  return (
    <PaletteDialog
      commands={buildCommands({ databases, selectedDB, onSelectDB, user, authMode })}
      onDone={next => setMode(next || null)} />
  )
}

// PaletteDialog ranks the commands by the query; it mounts empty each time
// the palette opens. onDone receives the mode a command opens, if any.
function PaletteDialog({ commands, onDone }) {
  const [query, setQuery] = useState('')
  const [index, setIndex] = useState(0)
  const inputRef = useRef(null)
  useEffect(() => {
    requestAnimationFrame(() => inputRef.current?.focus())
  }, [])

  const ranked = commands
    .map(c => ({ c, s: score(query, c.label + ' ' + c.hint) }))
    .filter(x => x.s > 0)
    .sort((a, b) => b.s - a.s)
    .map(x => x.c)
  const pick = i => {
    const cmd = ranked[i]
    if (!cmd) return
    cmd.run?.()
    onDone(cmd.opens)
  }
  const onKeyDown = e => {
    const shortcut = shortcutFor(e)
    if (shortcut === 'move') {
      e.preventDefault()
      setIndex(i => (e.key === 'ArrowDown'
        ? Math.min(i + 1, ranked.length - 1) : Math.max(i - 1, 0)))
    } else if (shortcut === 'run') {
      e.preventDefault()
      pick(index)
    }
  }

  return (
    <PaletteFrame onClose={() => onDone(null)}>
      <div className="flex items-center gap-2 p-3"
        style={{ borderBottom: '1px solid var(--border)' }}>
        <Search size={16} style={{ color: 'var(--text-secondary)' }} />
        <input ref={inputRef} data-testid="command-palette-input" value={query}
          onChange={e => { setQuery(e.target.value); setIndex(0) }}
          onKeyDown={onKeyDown} placeholder="Type a command or search..."
          className="flex-1 bg-transparent outline-none text-sm"
          style={{ color: 'var(--text-primary)' }} />
        <span className="text-xs px-1.5 py-0.5 rounded"
          style={{ background: 'var(--bg-hover)', color: 'var(--text-secondary)' }}>Esc</span>
      </div>
      <CommandResults ranked={ranked} index={index} onHover={setIndex} onPick={pick} />
    </PaletteFrame>
  )
}

function PaletteFrame({ onClose, children }) {
  return (
    <div data-testid="command-palette" role="dialog" aria-modal="true"
      aria-label="Command palette" onClick={onClose}
      style={{
        position: 'fixed', inset: 0, zIndex: 50, background: 'rgba(0,0,0,0.5)',
        display: 'flex', justifyContent: 'center', paddingTop: '12vh',
      }}>
      <div onClick={e => e.stopPropagation()} className="rounded"
        style={{
          width: '100%', maxWidth: 560, background: 'var(--bg-card)',
          border: '1px solid var(--border)', boxShadow: '0 10px 40px rgba(0,0,0,0.5)',
          maxHeight: '70vh', display: 'flex', flexDirection: 'column',
        }}>
        {children}
      </div>
    </div>
  )
}

function CommandResults({ ranked, index, onHover, onPick }) {
  return (
    <div className="overflow-auto" style={{ flex: 1 }}>
      {ranked.length === 0 && (
        <div className="p-4 text-sm text-center" style={{ color: 'var(--text-secondary)' }}>
          No matches
        </div>
      )}
      {ranked.map((c, i) => (
        <button key={c.id} type="button" data-testid={`command-${c.id}`}
          onMouseEnter={() => onHover(i)} onClick={() => onPick(i)}
          className="w-full flex items-center justify-between px-3 py-2 text-sm text-left"
          style={{
            background: i === index ? 'var(--bg-hover)' : 'transparent',
            color: 'var(--text-primary)',
          }}>
          <span>{c.label}</span>
          <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>{c.hint}</span>
        </button>
      ))}
    </div>
  )
}

