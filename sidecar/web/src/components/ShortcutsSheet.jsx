import { useEffect, useRef } from 'react'
import { APPROVAL_KEYMAP, KEYMAP } from './keymap'

// ShortcutsRows renders one row per keymap entry: its keys, then what it does.
function ShortcutsRows({ entries, testId }) {
  return (
    <dl className="text-sm">
      {entries.map(entry => (
        <div key={entry.id} data-testid={testId}
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
  )
}

// GLOSSARY_DOC defines the words the dashboard uses (ux-06a).
export const GLOSSARY_DOC =
  'https://github.com/jasonmassie01/pg_sage/blob/master/docs/glossary.md'

// ShortcutsSheet lists the keyboard shortcuts from the keymaps the key
// handlers use (dashboard-06): the global keys, then the approval list's
// verbs (ux-05a). `?` opens it; Escape, the Close button or a click outside
// closes it.
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
        <ShortcutsRows entries={KEYMAP} testId="shortcut-row" />
        <div data-testid="approval-shortcuts" className="mt-3 pt-3"
          style={{ borderTop: '1px solid var(--border)' }}>
          <h3 className="text-xs font-semibold mb-1" style={{ color: 'var(--text-secondary)' }}>
            On the Waiting on you cards
          </h3>
          <ShortcutsRows entries={APPROVAL_KEYMAP} testId="approval-shortcut-row" />
        </div>
        <p className="mt-3 text-xs" style={{ color: 'var(--text-secondary)' }}>
          The words pg_sage uses, such as Issue, Change and Autonomy, are defined in
          the <a href={GLOSSARY_DOC} data-testid="shortcuts-glossary" className="underline"
            target="_blank" rel="noreferrer" style={{ color: 'var(--accent)' }}>glossary</a>.
        </p>
      </div>
    </div>
  )
}
