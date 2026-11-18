import { useEffect, useState } from 'react'
import { apiGet } from '../lib/api'
import { formatTime } from '../lib/time'
import { ErrorBanner } from './ErrorBanner'
import {
  STATUS_LABELS, alsoFailing, firstFailing, linkLabel, statusOf, summaryOf, whyNotURL,
} from './whyNot'

// WhyNotPanel (ux-08b) answers "why has pg_sage not done this?" with
// ux-08a's ten-link chain from GET /api/v1/whynot: one row per link, the
// first failure highlighted, one button or copyable statement per fix.
// One read per question; a newer question drops an older answer, and a
// failed read shows the error with no chain. Operators only (the route
// refuses viewers); it records nothing.

const muted = { color: 'var(--text-secondary)' }
const accent = { color: 'var(--accent)' }
const STATUS_STYLES = {
  pass: { color: 'var(--green)', border: '1px solid var(--green)' },
  fail: { color: 'var(--red)', border: '1px solid var(--red)' },
  unknown: { color: 'var(--yellow)', border: '1px dashed var(--yellow)' },
  not_applicable: { color: 'var(--text-secondary)', border: '1px solid var(--border)' },
}

// useWhyNot reads url once; an answer for an older url is dropped.
function useWhyNot(url) {
  const [state, setState] = useState({ url: null, data: null, error: null })
  useEffect(() => {
    if (!url) return undefined
    const controller = new AbortController()
    let live = true
    apiGet(url, { signal: controller.signal })
      .then(data => { if (live) setState({ url, data, error: null }) })
      .catch(err => {
        if (live && err?.name !== 'AbortError') {
          setState({ url, data: null, error: err?.message || 'the read failed' })
        }
      })
    return () => {
      live = false
      controller.abort()
    }
  }, [url])
  return state.url === url ? state : { data: null, error: null }
}

function CopyButton({ text }) {
  const [copied, setCopied] = useState('')
  const copy = () => navigator.clipboard.writeText(text)
    .then(() => setCopied('Copied'))
    .catch(err => setCopied(`Copy failed (${err?.message || 'no clipboard'}); select it`))
  return <button type="button" onClick={copy} className="underline text-xs" style={accent}>
    {copied || 'Copy'}</button>
}

function Statement({ value, children }) {
  return <span className="flex flex-wrap items-center gap-2">
    <code className="px-1 rounded" style={{ background: 'var(--bg-hover)' }}>{value}</code>
    <CopyButton text={value} />{children}
  </span>
}

// FixAction is one typed fix; a kind this release does not know shows
// nothing beyond the link's sentence.
function FixAction({ fix }) {
  const value = typeof fix?.value === 'string' ? fix.value : ''
  if (!value) return null
  switch (fix.kind) {
    case 'grant_sql': return <Statement value={value} />
    case 'setting': return <Statement value={value}>
      <a href="#/settings" className="underline text-xs" style={accent}>Open Settings</a>
    </Statement>
    case 'ui_link': return value.startsWith('#/')
      ? <a href={value} className="underline text-xs" style={accent}>Open</a> : null
    case 'wait_until': {
      const at = new Date(value)
      return Number.isNaN(at.getTime()) ? null
        : <span data-testid="why-not-wait" className="text-xs">Clears at {formatTime(at)}</span>
    }
    default: return null
  }
}

function LinkRow({ link, first }) {
  const status = statusOf(link)
  return (
    <li data-testid="why-not-link" data-link={link.name} data-status={status}
      data-first-fail={first ? 'true' : 'false'} aria-current={first ? 'step' : undefined}
      className="rounded p-2 text-sm space-y-1" style={{ border: first
        ? '2px solid var(--red)' : '1px solid var(--border)' }}>
      <div className="flex flex-wrap items-baseline gap-2">
        <span data-testid="why-not-status" className="px-1 rounded text-xs"
          style={STATUS_STYLES[status]}>{STATUS_LABELS[status]}</span>
        <span className="font-medium" style={{ color: 'var(--text-primary)' }}>
          {linkLabel(link.name)}</span>
        <span style={muted}>{link.sentence}</span>
        {status === 'unknown' && link.error && <span className="text-xs" style={muted}>
          ({link.error})</span>}
      </div>
      <FixAction fix={link.fix} />
      {link.fix?.note && <p className="text-xs" style={muted}>{link.fix.note}</p>}
      {!link.fix && link.no_fix && <p className="text-xs" style={muted}>
        No fix: {link.no_fix}</p>}
    </li>
  )
}

function Chain({ data }) {
  const links = Array.isArray(data?.links) ? data.links : []
  const first = firstFailing(data)
  const others = alsoFailing(data)
  return <>
    <p data-testid="why-not-summary" className="text-sm font-medium"
      style={{ color: 'var(--text-primary)' }}>{summaryOf(data)}</p>
    {others.length > 0 && <p className="text-xs" style={muted}>
      Also failing: {others.join(', ')}.</p>}
    <ol className="space-y-1">
      {links.map(link => <LinkRow key={link.name} link={link} first={link === first} />)}
    </ol>
  </>
}

export function WhyNotPanel({ database, findingId, object, category }) {
  const url = whyNotURL({ database, findingId, object, category })
  const { data, error } = useWhyNot(url)
  let body
  if (!url) {
    body = <p data-testid="why-not-pick-database" className="text-sm" style={muted}>
      Pick one database to see why nothing has run there.</p>
  } else if (error) body = <ErrorBanner message={`Why not could not be read: ${error}`} />
  else if (!data) body = <p className="text-sm" style={muted}>Reading the checks…</p>
  else body = <Chain data={data} />
  return <section data-testid="why-not-panel" aria-label="Why not"
    className="mt-2 space-y-2 rounded p-3"
    style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
    {body}
  </section>
}

const mayAsk = user => !user || user.role === 'admin' || user.role === 'operator'

// WhyNotButton opens and closes the panel for one question. A viewer is
// not offered it: the route refuses viewers.
export function WhyNotButton({ user, label = 'Why not?', ...question }) {
  const [open, setOpen] = useState(false)
  if (!mayAsk(user)) return null
  return <div className="mt-1">
    <button type="button" aria-expanded={open} onClick={() => setOpen(!open)}
      className="underline text-xs" style={accent}>{label}</button>
    {open && <WhyNotPanel {...question} />}
  </div>
}
