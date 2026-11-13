import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CaseTicket } from './CaseTicket'
import { stubFetch } from '../../test/fetchStub'

// market-14: a case links to one Jira issue. Its status and sync error
// are shown as evidence; operators create the ticket. A case with no
// ticket shows no ticket row.
// No concurrent access tests: the one-ticket guarantee is the server's
// unique key; the button disables itself while its request runs.

afterEach(() => vi.unstubAllGlobals())

const caseRow = {
  id: 'finding:db1:bloat:table:t', identity_key: 'finding:db1:bloat:table:t',
  database_name: 'db1', state: 'open', title: 'Table t is bloated',
}
const operator = { role: 'operator' }
const viewer = { role: 'viewer' }
const ticket = {
  id: 1, database: 'db1', case_identity_key: 'finding:db1:bloat:table:t', system: 'jira',
  external_key: 'OPS-7', url: 'https://acme.atlassian.net/browse/OPS-7',
  status: 'In Progress', status_category: 'indeterminate',
}

describe('CaseTicket', () => {
  it('shows the linked ticket with its status', () => {
    render(<CaseTicket caseRow={caseRow} user={viewer} tickets={[ticket]} jiraEnabled />)
    const link = screen.getByRole('link', { name: /OPS-7/ })
    expect(link).toHaveAttribute('href', 'https://acme.atlassian.net/browse/OPS-7')
    expect(screen.getByTestId('case-ticket')).toHaveTextContent('In Progress')
    expect(screen.queryByTestId('case-ticket-sync-error')).toBeNull()
    expect(screen.queryByTestId('create-ticket')).toBeNull()
  })

  it('shows a sync failure as ticket_sync: unauthorized', () => {
    render(<CaseTicket caseRow={caseRow} user={viewer} jiraEnabled
      tickets={[{ ...ticket, sync_error: 'unauthorized' }]} />)
    expect(screen.getByTestId('case-ticket-sync-error'))
      .toHaveTextContent('ticket_sync: unauthorized')
  })

  it('ignores a ticket of another case or database', () => {
    render(<CaseTicket caseRow={caseRow} user={viewer} jiraEnabled
      tickets={[{ ...ticket, database: 'db2' }, { ...ticket, case_identity_key: 'x' }]} />)
    expect(screen.queryByTestId('case-ticket')).toBeNull()
  })

  it('shows nothing without a ticket to a viewer, or without Jira', () => {
    const { container, rerender } = render(
      <CaseTicket caseRow={caseRow} user={viewer} tickets={[]} jiraEnabled />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CaseTicket caseRow={caseRow} user={operator} tickets={undefined}
      jiraEnabled={false} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('creates the ticket for an operator with the case link', async () => {
    const onCreated = vi.fn()
    const fetchFn = stubFetch({
      'POST /api/v1/case-tickets': { status: 201, json: { ticket, created: true } },
    })
    render(<CaseTicket caseRow={caseRow} user={operator} tickets={[]} jiraEnabled
      onCreated={onCreated} />)
    fireEvent.click(screen.getByTestId('create-ticket'))
    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1))
    const body = JSON.parse(fetchFn.mock.calls[0][1].body)
    expect(body.database).toBe('db1')
    expect(body.case_id).toBe('finding:db1:bloat:table:t')
    expect(body.case_url).toMatch(/#\/cases\?case=finding%3Adb1%3Abloat%3Atable%3At&database=db1$/)
  })

  it('shows the API error and keeps the button', async () => {
    stubFetch({
      'POST /api/v1/case-tickets': { status: 502,
        json: { error: 'Jira refused the credentials (ticket_sync: unauthorized)' } },
    })
    render(<CaseTicket caseRow={caseRow} user={operator} tickets={[]} jiraEnabled />)
    fireEvent.click(screen.getByTestId('create-ticket'))
    expect(await screen.findByRole('alert')).toHaveTextContent('ticket_sync: unauthorized')
    expect(screen.getByTestId('create-ticket')).not.toBeDisabled()
  })

  it('offers no create on a resolved case', () => {
    const { container } = render(<CaseTicket caseRow={{ ...caseRow, state: 'resolved' }}
      user={operator} tickets={[]} jiraEnabled />)
    expect(container).toBeEmptyDOMElement()
  })
})
