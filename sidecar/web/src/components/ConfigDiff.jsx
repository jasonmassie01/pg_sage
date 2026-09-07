/* eslint-disable react-refresh/only-export-components */
import { useEffect } from 'react'
import { FileDiff, ArrowRight } from 'lucide-react'

// Compare edits against current config and return an array of
// { key, before, after, source } rows — one per changed field.
export function buildDiffRows(edits, cfg) {
  const rows = []
  for (const key of Object.keys(edits)) {
    const before = cfg[key]?.value
    const after = edits[key]
    const source = cfg[key]?.source ?? 'default'
    rows.push({ key, before, after, source })
  }
  rows.sort((a, b) => a.key.localeCompare(b.key))
  return rows
}

function renderValue(v) {
  if (v === null || v === undefined || v === '') {
    return <span style={{ color: 'var(--text-secondary)' }}>(unset)</span>
  }
  if (typeof v === 'boolean') return v ? 'true' : 'false'
  if (typeof v === 'object') {
    try {
      return JSON.stringify(v)
    } catch {
      return String(v)
    }
  }
  return String(v)
}

export function ConfigDiff({
  edits, cfg, saving, onConfirm, onCancel,
}) {
  useEffect(() => {
    const handler = e => {
      if (e.key === 'Escape' && !saving) onCancel()
    }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [onCancel, saving])

  const rows = buildDiffRows(edits, cfg)

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center"
      role="dialog" aria-modal="true"
      data-testid="config-diff-modal"
      style={{ background: 'rgba(0,0,0,0.5)' }}
      onClick={() => { if (!saving) onCancel() }}>
      <div className="rounded max-w-2xl w-full mx-4 max-h-[85vh]
        flex flex-col"
        onClick={e => e.stopPropagation()}
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <div className="p-5 border-b"
          style={{ borderColor: 'var(--border)' }}>
          <div className="flex items-center gap-2">
            <FileDiff size={20} style={{ color: 'var(--accent)' }} />
            <h3 className="text-sm font-semibold"
              style={{ color: 'var(--text-primary)' }}>
              Review Configuration Changes
            </h3>
          </div>
          <p className="text-xs mt-1"
            style={{ color: 'var(--text-secondary)' }}>
            {rows.length} key{rows.length === 1 ? '' : 's'} will change.
            Review the diff before saving.
          </p>
        </div>

        <div className="overflow-y-auto flex-1 p-5 space-y-3"
          data-testid="config-diff-rows">
          {rows.length === 0 && (
            <div className="text-sm"
              style={{ color: 'var(--text-secondary)' }}>
              No changes to save.
            </div>
          )}
          {rows.map(r => (
            <div key={r.key}
              data-testid={`config-diff-row-${r.key}`}
              className="rounded p-3"
              style={{
                background: 'var(--bg-primary)',
                border: '1px solid var(--border)',
              }}>
              <div className="flex items-center justify-between mb-2">
                <code className="text-xs font-mono"
                  style={{ color: 'var(--text-primary)' }}>
                  {r.key}
                </code>
                <span className="text-[10px] px-1.5 py-0.5 rounded"
                  style={{
                    color: 'var(--text-secondary)',
                    border: '1px solid var(--border)',
                  }}>
                  from: {r.source}
                </span>
              </div>
              <div className="flex items-start gap-3 text-xs font-mono">
                <div className="flex-1 rounded px-2 py-1"
                  data-testid={`config-diff-before-${r.key}`}
                  style={{
                    background: 'rgba(239, 68, 68, 0.08)',
                    color: 'var(--red)',
                    border: '1px solid rgba(239, 68, 68, 0.25)',
                    wordBreak: 'break-all',
                  }}>
                  {renderValue(r.before)}
                </div>
                <ArrowRight size={14}
                  style={{
                    color: 'var(--text-secondary)',
                    marginTop: 4,
                    flexShrink: 0,
                  }} />
                <div className="flex-1 rounded px-2 py-1"
                  data-testid={`config-diff-after-${r.key}`}
                  style={{
                    background: 'rgba(34, 197, 94, 0.08)',
                    color: 'var(--green)',
                    border: '1px solid rgba(34, 197, 94, 0.25)',
                    wordBreak: 'break-all',
                  }}>
                  {renderValue(r.after)}
                </div>
              </div>
            </div>
          ))}
        </div>

        <div className="p-4 border-t flex gap-2 justify-end"
          style={{ borderColor: 'var(--border)' }}>
          <button onClick={onCancel}
            disabled={saving}
            data-testid="config-diff-cancel"
            className="px-3 py-1.5 rounded text-sm"
            style={{
              background: 'var(--bg-card)',
              color: 'var(--text-secondary)',
              border: '1px solid var(--border)',
              opacity: saving ? 0.6 : 1,
            }}>
            Cancel
          </button>
          <button onClick={onConfirm}
            disabled={saving || rows.length === 0}
            data-testid="config-diff-confirm"
            className="px-3 py-1.5 rounded text-sm font-medium"
            style={{
              background: 'var(--accent)',
              color: '#fff',
              opacity: (saving || rows.length === 0) ? 0.6 : 1,
            }}>
            {saving ? 'Saving...' : `Apply ${rows.length} Change${rows.length === 1 ? '' : 's'}`}
          </button>
        </div>
      </div>
    </div>
  )
}
