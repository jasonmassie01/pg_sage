// A case's Jira ticket (market-14). The status is read back from Jira as
// evidence only: closing the issue never resolves the case. Operators
// create the ticket; one case has at most one.
import { useState } from 'react'
import { caseID, caseLink } from './casePresentation'

function canCreate(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

function ticketFor(tickets, caseRow) {
  return (tickets || []).find(t => t.database === caseRow.database_name &&
    t.case_identity_key === caseRow.identity_key) || null
}

function caseURL(caseRow) {
  const { origin, pathname } = window.location
  return `${origin}${pathname}${caseLink('cases', caseRow)}`
}

async function createTicket(caseRow) {
  const res = await fetch('/api/v1/case-tickets', {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ database: caseRow.database_name, case_id: caseID(caseRow),
      case_url: caseURL(caseRow) }),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({}))
    throw new Error(err.error || `Could not create the ticket (${res.status})`)
  }
  return res.json()
}

function TicketRow({ ticket }) {
  return (
    <div className="mt-2 text-xs flex gap-2 flex-wrap items-center" data-testid="case-ticket"
      style={{ color: 'var(--text-secondary)' }}>
      <span>Ticket</span>
      <a href={ticket.url} target="_blank" rel="noreferrer" className="underline"
        style={{ color: 'var(--accent)' }}>Jira {ticket.external_key}</a>
      <span>{ticket.status || 'status not read yet'}</span>
      {ticket.sync_error && (
        <span data-testid="case-ticket-sync-error" style={{ color: 'var(--red)' }}>
          ticket_sync: {ticket.sync_error}
        </span>
      )}
    </div>
  )
}

function CreateTicket({ caseRow, onCreated }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function onClick() {
    setBusy(true)
    setError(null)
    try {
      await createTicket(caseRow)
      if (onCreated) onCreated()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="mt-2 text-xs">
      <button type="button" disabled={busy} onClick={onClick} data-testid="create-ticket"
        className="px-2 py-1 rounded"
        style={{ color: 'var(--text-primary)', border: '1px solid var(--border)' }}>
        {busy ? 'Creating ticket...' : 'Create Jira ticket'}
      </button>
      {error && <p role="alert" className="mt-1" style={{ color: 'var(--red)' }}>{error}</p>}
    </div>
  )
}

export function CaseTicket({ caseRow, user, tickets, jiraEnabled, onCreated }) {
  const ticket = ticketFor(tickets, caseRow)
  if (ticket) return <TicketRow ticket={ticket} />
  if (!jiraEnabled || !canCreate(user) || caseRow.state !== 'open') return null
  return <CreateTicket caseRow={caseRow} onCreated={onCreated} />
}
