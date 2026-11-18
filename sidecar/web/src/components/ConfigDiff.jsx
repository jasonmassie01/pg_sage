/* eslint-disable react-refresh/only-export-components */
import { useEffect, useRef, useState } from 'react'
import { Dialog } from './Dialog'
import { FileDiff, ArrowRight, Copy, X } from 'lucide-react'

// ConfigDiff is the review step before a Settings save. It runs on the
// shared Dialog (dashboard-10c): focus stays inside, starts on Cancel and
// returns to Review & Save; Escape cancels unless a save is in flight. A
// failed save shows inside the open dialog and keeps the diff, and a diff
// taller than its box is a Tab stop. The caller mounts it only while open.

// Secret fields (API keys, passwords, routing keys, tokens) are never
// rendered in the review modal (G9-B25).
const SECRET_KEY = /(api_key|password|routing_key|secret|token|encryption_key)$/
const SECRET_MASK = '•••• (changed)'

export function isSecretConfigKey(key) {
  return SECRET_KEY.test(key)
}

// Compare edits against current config and return an array of
// { key, before, after, source } rows — one per changed field.
export function buildDiffRows(edits, cfg) {
  const rows = []
  for (const key of Object.keys(edits)) {
    const secret = isSecretConfigKey(key)
    const before = secret ? SECRET_MASK : cfg[key]?.value
    const after = secret ? SECRET_MASK : edits[key]
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

function DiffRow({ row }) {
  return (
    <div data-testid={`config-diff-row-${row.key}`} className="rounded p-3"
      style={{ background: 'var(--bg-primary)', border: '1px solid var(--border)' }}>
      <div className="flex items-center justify-between mb-2">
        <code className="text-xs font-mono" style={{ color: 'var(--text-primary)' }}>
          {row.key}
        </code>
        <span className="text-[10px] px-1.5 py-0.5 rounded"
          style={{ color: 'var(--text-secondary)', border: '1px solid var(--border)' }}>
          from: {row.source}
        </span>
      </div>
      <div className="flex items-start gap-3 text-xs font-mono">
        <div className="flex-1 rounded px-2 py-1"
          data-testid={`config-diff-before-${row.key}`}
          style={{
            background: 'rgba(239, 68, 68, 0.08)', color: 'var(--red)',
            border: '1px solid rgba(239, 68, 68, 0.25)', wordBreak: 'break-all',
          }}>
          {renderValue(row.before)}
        </div>
        <ArrowRight size={14} aria-hidden="true"
          style={{ color: 'var(--text-secondary)', marginTop: 4, flexShrink: 0 }} />
        <div className="flex-1 rounded px-2 py-1"
          data-testid={`config-diff-after-${row.key}`}
          style={{
            background: 'rgba(34, 197, 94, 0.08)', color: 'var(--green)',
            border: '1px solid rgba(34, 197, 94, 0.25)', wordBreak: 'break-all',
          }}>
          {renderValue(row.after)}
        </div>
      </div>
    </div>
  )
}

// useOverflows reports whether the element's content is taller than its box.
// An overflowing diff becomes a named Tab stop, so a keyboard user can scroll
// every row before applying (the Tab wrap reaches only focusable elements).
// It measures in an effect, which runs after Dialog's showModal() effect.
function useOverflows(ref, count) {
  const [overflows, setOverflows] = useState(false)
  useEffect(() => {
    const el = ref.current
    if (!el) return undefined
    const check = () => setOverflows(el.scrollHeight > el.clientHeight)
    check()
    window.addEventListener('resize', check)
    return () => window.removeEventListener('resize', check)
  }, [ref, count])
  return overflows
}

function Title() {
  return (
    <span className="flex items-center gap-2">
      <FileDiff size={20} aria-hidden="true" style={{ color: 'var(--accent)' }} />
      Review Configuration Changes
    </span>
  )
}

function Buttons({ count, saving, onConfirm, onCancel }) {
  const blocked = saving || count === 0
  return (
    <div className="flex gap-2 justify-end">
      <button type="button" onClick={onCancel} disabled={saving} data-autofocus
        data-testid="config-diff-cancel" className="px-3 py-1.5 rounded text-sm"
        style={{
          background: 'var(--bg-card)', color: 'var(--text-secondary)',
          border: '1px solid var(--border)', opacity: saving ? 0.6 : 1,
        }}>
        Cancel
      </button>
      <button type="button" onClick={onConfirm} disabled={blocked}
        data-testid="config-diff-confirm" className="px-3 py-1.5 rounded text-sm font-medium"
        style={{ background: 'var(--accent)', color: '#fff', opacity: blocked ? 0.6 : 1 }}>
        {saving ? 'Saving...' : `Apply ${count} Change${count === 1 ? '' : 's'}`}
      </button>
    </div>
  )
}

export function ConfigDiff({
  edits, cfg, saving, error, onConfirm, onCancel,
}) {
  const rows = buildDiffRows(edits, cfg)
  const rowsRef = useRef(null)
  const scrollable = useOverflows(rowsRef, rows.length)
  const confirm = () => { if (!saving && rows.length > 0) onConfirm() }
  return (
    <Dialog open title={<Title />} onClose={onCancel} busy={saving}
      testId="config-diff-modal" width={672}>
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {rows.length} key{rows.length === 1 ? '' : 's'} will change.
        Review the diff before saving.
      </p>
      <div ref={rowsRef} data-testid="config-diff-rows"
        className="max-h-[50vh] overflow-y-auto overscroll-contain space-y-3 pr-1"
        {...(scrollable
          ? { tabIndex: 0, role: 'region', 'aria-label': 'Configuration changes' }
          : {})}>
        {rows.length === 0 && (
          <div className="text-sm" style={{ color: 'var(--text-secondary)' }}>
            No changes to save.
          </div>
        )}
        {rows.map(r => <DiffRow key={r.key} row={r} />)}
      </div>
      {error && (
        <div role="alert" data-testid="config-diff-error" className="text-sm p-2 rounded"
          style={{ color: 'var(--red)', border: '1px solid var(--red)' }}>
          {error}
        </div>
      )}
      <Buttons count={rows.length} saving={saving} onConfirm={confirm} onCancel={onCancel} />
    </Dialog>
  )
}

// cli-01b: after a Settings save, the saved change as the YAML a config
// file would hold (GET /api/v1/config/yaml, the same route as `pg_sage_sidecar
// config export`; secrets come back as ${NAME} placeholders). A save is a
// stored override that wins over the file, so this says so and offers the
// YAML to copy into the file. It fetches only when asked.

export function savedYAMLUrl(keys) {
  return `/api/v1/config/yaml?keys=${encodeURIComponent(keys.join(','))}`
}

async function fetchSavedYAML(keys) {
  const res = await fetch(savedYAMLUrl(keys), { credentials: 'include' })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.error || `YAML unavailable (HTTP ${res.status})`)
  if (typeof body.yaml !== 'string') throw new Error('YAML unavailable')
  return body.yaml
}

// The state belongs to one save (its keys array): a later save, even of the
// same keys, starts empty, and an answer for an earlier save is dropped.
const NO_YAML = { yaml: null, error: null, loading: false }

function useSavedYAML(keys) {
  const [state, setState] = useState({ ...NO_YAML, keys: null })
  const load = async () => {
    setState({ ...NO_YAML, loading: true, keys })
    const settle = next => setState(prev => (prev.keys === keys ? { ...next, keys } : prev))
    try {
      settle({ ...NO_YAML, yaml: await fetchSavedYAML(keys) })
    } catch (e) {
      settle({ ...NO_YAML, error: e.message || 'YAML unavailable' })
    }
  }
  return [state.keys === keys ? state : NO_YAML, load]
}

function CopyYAML({ yaml }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(yaml)
      setCopied(true)
    } catch {
      setCopied(false) // clipboard unavailable: the text stays selectable
    }
  }
  return (
    <button type="button" onClick={copy} data-testid="config-saved-yaml-copy"
      className="flex items-center gap-1 px-2 py-1 rounded text-xs"
      style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
      <Copy size={12} aria-hidden="true" /> {copied ? 'Copied' : 'Copy'}
    </button>
  )
}

const SAVED_BOX = {
  background: 'var(--bg-card)', border: '1px solid var(--border)',
  color: 'var(--text-secondary)',
}

export function SavedYAML({ keys, onDismiss }) {
  const [state, load] = useSavedYAML(keys)
  if (!keys?.length) return null
  const n = keys.length
  return (
    <div data-testid="config-saved-yaml" className="rounded p-3 text-xs space-y-2"
      style={SAVED_BOX}>
      <div className="flex items-start justify-between gap-3">
        <span>
          Saved {n} setting{n === 1 ? '' : 's'} as a stored override, which wins over the
          config file until it is reset. To keep the file the source of truth, copy the
          equivalent YAML into it.
        </span>
        <button type="button" onClick={onDismiss} aria-label="Dismiss"
          data-testid="config-saved-yaml-dismiss"><X size={14} aria-hidden="true" /></button>
      </div>
      {!state.yaml && (
        <button type="button" onClick={load} disabled={state.loading}
          data-testid="config-saved-yaml-show" className="underline">
          {state.loading ? 'Loading...' : 'Show equivalent YAML'}
        </button>
      )}
      {state.error && <div role="alert" style={{ color: 'var(--red)' }}>{state.error}</div>}
      {state.yaml && (
        <div className="space-y-1">
          <pre data-testid="config-saved-yaml-text" className="font-mono p-2 rounded"
            style={{ background: 'var(--bg-primary)', color: 'var(--text-primary)' }}
          >{state.yaml}</pre>
          <CopyYAML yaml={state.yaml} />
        </div>
      )}
    </div>
  )
}
