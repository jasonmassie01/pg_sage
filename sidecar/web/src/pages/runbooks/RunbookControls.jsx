import { useState } from 'react'
import { Dialog } from '../../components/Dialog'
import { describeChange, diffDefinitions } from './runbookDiff'
import { canOperate } from './runbookStatus'

// Runbook detail controls (playbooks-10): which version runs, the version
// list, revising (a draft staged beside the signed version), signing after
// a structural diff from the newest signed version, and retiring only
// after the runbook's name is typed. Sign and retire confirm in the shared
// modal Dialog (ux-12).

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const box = { borderColor: 'var(--border)' }
const button = { ...strong, border: '1px solid var(--border)' }

function versionText(v, running) {
  const signature = v.signed_at
    ? `signed by ${v.signed_by} at ${v.signed_at} · ` +
      (v.signature_valid ? 'signature valid' : 'signature INVALID (never runs)')
    : 'unsigned draft'
  return `v${v.version} · hash ${v.content_hash} · ${signature}` +
    (v.version === running ? ' · running' : '')
}

export function VersionList({ rb }) {
  const versions = Array.isArray(rb.versions) ? rb.versions : []
  if (versions.length === 0) return null
  return (
    <div data-testid="runbook-versions" className="mt-2">
      <div style={strong}>Versions</div>
      <ul className="list-disc pl-4" style={muted}>
        {versions.map(v => (
          <li key={v.version} data-testid={`runbook-version-${v.version}`}
            className="break-all">
            {versionText(v, rb.running_version)}
          </li>
        ))}
      </ul>
    </div>
  )
}

// newestSigned is the newest version whose signature is valid: the base
// an admin reviews a draft against.
function newestSigned(rb) {
  const versions = Array.isArray(rb.versions) ? rb.versions : []
  return versions.find(v => v.signed_at && v.signature_valid) || rb.running || null
}

export function SignDialog({ rb, onSign, onCancel }) {
  const v = rb.latest
  const base = newestSigned(rb)
  const changes = base ? diffDefinitions(base.definition, v.definition) : []
  return (
    <Dialog open title="Sign runbook" onClose={onCancel} testId="runbook-sign-dialog"
      width={560}>
      <div style={strong}>Sign version {v.version}</div>
      {base ? (
        <div>
          <div style={muted}>Changes against v{base.version}, the newest signed version:</div>
          <ul data-testid="runbook-sign-diff" className="list-disc pl-4" style={muted}>
            {changes.length === 0 && <li>no structural changes</li>}
            {changes.map(c => <li key={describeChange(c)}>{describeChange(c)}</li>)}
          </ul>
        </div>
      ) : (
        <div style={muted}>First signature: no version of this runbook was signed before.</div>
      )}
      <div style={muted} className="break-all">
        Content hash: <code>{v.content_hash}</code>
      </div>
      <div className="flex gap-2">
        <button type="button" data-testid="runbook-sign-confirm" style={button}
          className="rounded px-2 py-1" onClick={onSign}>
          Sign
        </button>
        <button type="button" style={button} className="rounded px-2 py-1"
          onClick={onCancel}>
          Cancel
        </button>
      </div>
    </Dialog>
  )
}

export function RetireDialog({ name, onRetire, onCancel }) {
  const [typed, setTyped] = useState('')
  const matches = typed === name
  return (
    <Dialog open title="Retire runbook" onClose={onCancel} testId="runbook-retire-dialog">
      <div style={strong}>
        Retiring stops every version of {name} for good; it cannot be undone.
      </div>
      <label style={muted} className="block">
        Type the runbook name to confirm
        <input data-testid="runbook-retire-name" className="ml-2 rounded border p-1"
          style={box} value={typed} onChange={e => setTyped(e.target.value)} />
      </label>
      <div className="flex gap-2">
        <button type="button" data-testid="runbook-retire-confirm" style={button}
          className="rounded px-2 py-1" disabled={!matches}
          onClick={() => { if (matches) onRetire() }}>
          Retire for good
        </button>
        <button type="button" style={button} className="rounded px-2 py-1"
          onClick={onCancel}>
          Cancel
        </button>
      </div>
    </Dialog>
  )
}

function reviseNote(rb) {
  const next = rb.latest_version + 1
  if (rb.running_version > 0) {
    return `v${rb.running_version} keeps running; v${next} is a draft until an admin signs it`
  }
  return `v${next} is a draft until an admin signs it; the runbook does not run until then`
}

export function ReviseForm({ rb, onSubmit, onProblems, onCancel }) {
  const [json, setJSON] = useState(() => JSON.stringify(rb.latest.definition, null, 2))
  function submit() {
    let definition
    try {
      definition = JSON.parse(json)
    } catch (err) {
      onProblems({ message: `Revision is not valid JSON: ${err.message}`, items: [] })
      return
    }
    onSubmit({ base_version: rb.latest_version, definition })
  }
  return (
    <div data-testid="runbook-revise-form" className="mt-2 space-y-1 rounded border p-2"
      style={box}>
      <div style={strong}>Revise version {rb.latest_version}</div>
      <div data-testid="runbook-revise-note" style={muted}>{reviseNote(rb)}</div>
      <textarea data-testid="runbook-revise-json" className="w-full rounded border p-1"
        style={box} rows={10} value={json} onChange={e => setJSON(e.target.value)} />
      <div className="flex gap-2">
        <button type="button" data-testid="runbook-revise-submit" style={button}
          className="rounded px-2 py-1" onClick={submit}>
          Stage the draft
        </button>
        <button type="button" style={button} className="rounded px-2 py-1"
          onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  )
}

// RunbookActions are the role-gated buttons and the open form or dialog.
export function RunbookActions({ rb, user, open, setOpen, act, onProblems }) {
  const v = rb.latest
  const live = rb.status !== 'retired'
  const admin = user?.role === 'admin'
  return (
    <>
      <div className="mt-2 flex gap-2">
        {canOperate(user) && live && (
          <button type="button" data-testid="runbook-revise" style={button}
            className="rounded px-2 py-1" onClick={() => setOpen('revise')}>
            Revise
          </button>
        )}
        {admin && live && !v.signed_at && (
          <button type="button" data-testid="runbook-sign" style={button}
            className="rounded px-2 py-1" onClick={() => setOpen('sign')}>
            Review and sign version {v.version}
          </button>
        )}
        {admin && live && (
          <button type="button" data-testid="runbook-retire" style={button}
            className="rounded px-2 py-1" onClick={() => setOpen('retire')}>
            Retire
          </button>
        )}
      </div>
      {open === 'revise' && (
        <ReviseForm rb={rb} onProblems={onProblems} onCancel={() => setOpen(null)}
          onSubmit={body => act('versions', body,
            'Draft staged; the signed version keeps running until an admin signs it')} />
      )}
      {open === 'sign' && (
        <SignDialog rb={rb} onCancel={() => setOpen(null)}
          onSign={() => act('sign', { version: v.version, content_hash: v.content_hash },
            'Runbook signed')} />
      )}
      {open === 'retire' && (
        <RetireDialog name={v.name} onCancel={() => setOpen(null)}
          onRetire={() => act('retire', undefined, 'Runbook retired')} />
      )}
    </>
  )
}
