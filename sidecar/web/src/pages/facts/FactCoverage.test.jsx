import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FactCard } from './FactCard'

// facts-09: a proposed fact's card shows which objects its subject matches
// today, so the person confirming it sees what it would bind.

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

const proposed = {
  id: 31, database: 'orders', type: 'owned_by_app_migrations', subject_kind: 'index',
  subject: 'public.idx_thesis_*', value: {}, source: 'model', proposed_by: 'ask:user:1',
  evidence: [], rationale: '', status: 'proposed', proposals: 1,
  summary: "indexes public.idx_thesis_* are owned by the application's migrations",
}

const names = n => Array.from({ length: n }, (_, i) =>
  `public.idx_thesis_${String(i).padStart(2, '0')}`)

function stub(reply) {
  const mock = vi.fn(async () => reply)
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => vi.unstubAllGlobals())

describe('FactCard coverage', () => {
  it('fetches nothing until asked, then lists what the subject matches', async () => {
    const mock = stub(response(200, { objects: names(12), truncated: false }))
    render(<FactCard fact={proposed} canDecide onDecided={() => {}} />)
    expect(mock).not.toHaveBeenCalled()
    fireEvent.click(screen.getByTestId('fact-coverage-31-check'))
    const out = await screen.findByTestId('fact-coverage-31')
    expect(mock.mock.calls[0][0]).toBe(
      '/api/v1/facts/coverage?database=orders&kind=index&subject=public.idx_thesis_*')
    expect(mock.mock.calls[0][1].credentials).toBe('include')
    expect(out).toHaveTextContent('Matches 12 objects')
    expect(out).toHaveTextContent('public.idx_thesis_00')
    expect(out).toHaveTextContent('public.idx_thesis_09')
    expect(out).not.toHaveTextContent('public.idx_thesis_10')
    expect(out).toHaveTextContent('and 2 more')
  })

  it('says 50+ when the list was cut', async () => {
    stub(response(200, { objects: names(50), truncated: true }))
    render(<FactCard fact={proposed} canDecide={false} onDecided={() => {}} />)
    fireEvent.click(screen.getByTestId('fact-coverage-31-check'))
    const out = await screen.findByTestId('fact-coverage-31')
    expect(out).toHaveTextContent('Matches 50+ objects')
    expect(out).toHaveTextContent('and more')
  })

  it('says when nothing matches today', async () => {
    stub(response(200, { objects: [], truncated: false }))
    render(<FactCard fact={proposed} canDecide onDecided={() => {}} />)
    fireEvent.click(screen.getByTestId('fact-coverage-31-check'))
    expect(await screen.findByTestId('fact-coverage-31')).toHaveTextContent(
      'Matches no objects today')
  })

  it("shows the API's refusal", async () => {
    stub(response(400, { error: 'subject could match schema sage',
      code: 'protected_subject' }))
    render(<FactCard fact={proposed} canDecide onDecided={() => {}} />)
    fireEvent.click(screen.getByTestId('fact-coverage-31-check'))
    expect(await screen.findByTestId('fact-coverage-31-error')).toHaveTextContent(
      'subject could match schema sage')
    expect(screen.queryByTestId('fact-coverage-31')).toBeNull()
  })

  it('treats a malformed reply as an error, not as no match', async () => {
    stub(response(200, { facts: [] }))
    render(<FactCard fact={proposed} canDecide onDecided={() => {}} />)
    fireEvent.click(screen.getByTestId('fact-coverage-31-check'))
    expect(await screen.findByTestId('fact-coverage-31-error')).toHaveTextContent(
      'unreadable')
  })

  it('offers coverage only for proposed facts', () => {
    stub(response(200, { objects: [], truncated: false }))
    render(<FactCard fact={{ ...proposed, status: 'confirmed' }} canDecide
      onDecided={() => {}} />)
    expect(screen.queryByTestId('fact-coverage-31-check')).toBeNull()
  })
})
