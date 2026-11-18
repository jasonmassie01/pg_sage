import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AskPage } from './AskPage'
import { makeAnswer, operator, postBodies, posts, response, stubAsk } from './ask/testkit'

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
    expect(input.value).not.toContain('Do not create')
    expect(screen.getByRole('link', { name: 'Back to this issue' })).toHaveAttribute('href', '#/cases?case=case-17&database=orders')
    expect(posts(mock)).toHaveLength(0)
    fireEvent.click(screen.getByTestId('ask-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(postBodies(mock)[0].read_only).toBe(true)
    expect(postBodies(mock)[0].question).not.toContain('Do not create')
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

  it('retains finding identity for a long technical case without a prompt-text restriction', async () => {
    const id = 'finding:orders:slow_query:' + 'x'.repeat(3000)
    casesFetch([{ id, database_name: 'orders', source_type: 'finding', source_ids: ['17'], title: 'x'.repeat(3000) }])
    render(<AskPage database="orders" caseId={id} user={operator} />)
    const input = await screen.findByTestId('ask-input')
    expect(input.value).toMatch(/^Explain this case using cited evidence/)
    expect(input.value).toContain('finding 17')
    expect(input.value.length).toBeLessThanOrEqual(2000)
    expect(input.value).toContain('[shortened]')
  })

  it('posts read_only for every question while a case is open and shows Answer only', async () => {
    let n = 0
    const ask = stubAsk({ ask: (_url, init) => {
      n += 1
      return response(200, makeAnswer({ id: 50 + n,
        read_only: JSON.parse(init.body).read_only === true }))
    } })
    const mock = vi.fn((url, init) => String(url).startsWith('/api/v1/cases?')
      ? Promise.resolve(response(200, { cases: [{ id: 'case-17', database_name: 'orders', source_type: 'finding', source_ids: ['17'], title: 'Slow query' }] }))
      : ask(url, init))
    vi.stubGlobal('fetch', mock)
    render(<AskPage database="orders" caseId="case-17" user={operator} />)
    await screen.findByTestId('ask-input')
    fireEvent.click(screen.getByTestId('ask-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(await screen.findByTestId('ask-read-only')).toHaveTextContent('Answer only')
    fireEvent.change(screen.getByTestId('ask-input'), { target: { value: 'And what next?' } })
    fireEvent.click(screen.getByTestId('ask-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(2))
    expect(postBodies(mock).map(b => b.read_only)).toEqual([true, true])
    expect(postBodies(mock)[1]).toEqual({ question: 'And what next?', conversation_id: 'c-1', read_only: true })
  })

  it('tells the operator the case explanation cannot queue anything', async () => {
    casesFetch([{ id: 'case-17', database_name: 'orders', source_type: 'finding', source_ids: ['17'], title: 'Slow query' }])
    render(<AskPage database="orders" caseId="case-17" user={operator} />)
    await screen.findByTestId('ask-input')
    expect(screen.getByTestId('ask-case-context')).toHaveTextContent('answer only')
  })

  it('keeps general Ask without read_only and without the Answer only badge', async () => {
    const mock = stubAsk()
    render(<AskPage database="orders" user={operator} />)
    fireEvent.change(await screen.findByTestId('ask-input'), { target: { value: 'Why?' } })
    fireEvent.click(screen.getByTestId('ask-submit'))
    await screen.findByTestId('ask-answer-41')
    expect(postBodies(mock)).toEqual([{ question: 'Why?' }])
    expect(screen.queryByTestId('ask-read-only')).toBeNull()
  })
})
