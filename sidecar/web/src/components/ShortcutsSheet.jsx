import { useEffect, useRef } from 'react'
import { KEYMAP } from './keymap'

// ShortcutsSheet lists the keyboard shortcuts from the keymap the command
// palette's key handlers use (dashboard-06). `?` opens it; Escape, the
// Close button or a click outside closes it.
export function ShortcutsSheet({ onClose }) {
  const closeRef = useRef(null)
  useEffect(() => {
    requestAnimationFrame(() => closeRef.current?.focus())
  }, [])

  return (
    <div data-testid="shortcuts-sheet" role="dialog" aria-modal="true"
      aria-label="Keyboard shortcuts" onClick={onClose}
      style={{
        position: 'fixed', inset: 0, zIndex: 50, background: 'rgba(0,0,0,0.5)',
        display: 'flex', justifyContent: 'center', paddingTop: '12vh',
      }}>
      <div onClick={e => e.stopPropagation()} className="rounded p-4"
        style={{
          width: '100%', maxWidth: 420, alignSelf: 'flex-start',
          background: 'var(--bg-card)', border: '1px solid var(--border)',
          boxShadow: '0 10px 40px rgba(0,0,0,0.5)', color: 'var(--text-primary)',
        }}>
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-sm font-semibold">Keyboard shortcuts</h2>
          <button ref={closeRef} type="button" onClick={onClose}
            className="text-xs px-2 py-1 rounded"
            style={{ background: 'var(--bg-hover)', color: 'var(--text-secondary)' }}>
            Close
          </button>
        </div>
        <dl className="text-sm">
          {KEYMAP.map(entry => (
            <div key={entry.id} data-testid="shortcut-row"
              className="flex items-center justify-between gap-4 py-1.5">
              <dt className="flex gap-1">
                {entry.keys.map(key => (
                  <kbd key={key} className="text-xs px-1.5 py-0.5 rounded"
                    style={{ background: 'var(--bg-hover)', color: 'var(--text-primary)' }}>
                    {key}
                  </kbd>
                ))}
              </dt>
              <dd style={{ color: 'var(--text-secondary)' }}>{entry.description}</dd>
            </div>
          ))}
        </dl>
      </div>
    </div>
  )
}
