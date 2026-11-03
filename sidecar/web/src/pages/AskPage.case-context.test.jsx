import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AskPage } from './AskPage'
import { operator, posts, response, stubAsk } from './ask/testkit'

afterEach(() => vi.unstubAllGlobals())

function casesFetch(cases) {
  const ask = stubAsk()
  const mock = vi.fn((url, init) => String(url).startsWith('/api/v1/cases?')
    ? Promise.resolve(response(200, { cases })) : ask(url, init))
  vi.stubGlobal('fetch', mock)
  return mock
}

describe('case-scoped Ask', () => {
  it('prefills an evidence-scoped read-only question without sending it', async () => {
    const mock = casesFetch([{ id: 'case-17', database_name: 'orders', source_type: 'finding', source_ids: ['17'], title: 'A query used temporary disk space' }])
    render(<AskPage database="orders" caseId="case-17" user={operator} />)
    const input = await screen.findByTestId('ask-input')
    expect(input.value).toContain('finding 17')
    expect(input.value).toContain('Do not create proposals')
    expect(screen.getByRole('link', { name: 'Back to this case' })).toHaveAttribute('href', '#/cases?case=case-17&database=orders')
    expect(posts(mock)).toHaveLength(0)
    fireEvent.click(screen.getByTestId('ask-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
  })

  it('withholds a mismatched case rather than borrowing another database’s evidence', async () => {
    const mock = casesFetch([{ id: 'case-17', database_name: 'billing', title: 'Private billing case' }])
    render(<AskPage database="orders" caseId="case-17" user={operator} />)
    expect(await screen.findByTestId('ask-input')).toHaveValue('')
    expect(screen.getByTestId('ask-case-context')).toHaveTextContent('could not be found')
    expect(screen.queryByText('Private billing case')).toBeNull()
    expect(posts(mock)).toHaveLength(0)
  })

  it('uses the public case ID consistently and distinguishes unavailable evidence from a missing case', async () => {
    const ask = stubAsk()
    let failed = true
    vi.stubGlobal('fetch', vi.fn((url, init) => String(url).startsWith('/api/v1/cases?')
      ? Promise.resolve(response(failed ? 503 : 200, { cases: [{ case_id: 'case-17', database_name: 'orders', source_type: 'finding', source_ids: ['17'], title: 'Reported condition' }] }))
      : ask(url, init)))
    render(<AskPage database="orders" caseId="case-17" user={operator} />)
    expect(await screen.findByRole('alert')).toHaveTextContent('could not be loaded')
    expect(screen.queryByText(/could not be found/)).toBeNull()
    failed = false
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() => expect(screen.getByTestId('ask-input').value).toContain('Case reference: case-17'))
    expect(screen.getByTestId('ask-input').value).not.toContain('undefined')
  })

  it('retains explanation-only restrictions and finding identity for a long technical case', async () => {
    const id = 'finding:orders:slow_query:' + 'x'.repeat(3000)
    casesFetch([{ id, database_name: 'orders', source_type: 'finding', source_ids: ['17'], title: 'x'.repeat(3000) }])
    render(<AskPage database="orders" caseId={id} user={operator} />)
    const input = await screen.findByTestId('ask-input')
    expect(input.value).toMatch(/^Do not create proposals, investigations, or actions/)
    expect(input.value).toContain('finding 17')
    expect(input.value.length).toBeLessThanOrEqual(2000)
    expect(input.value).toContain('[shortened]')
  })
})
