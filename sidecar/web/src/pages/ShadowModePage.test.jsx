import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ShadowModePage } from './ShadowModePage'
import { useAPI } from '../hooks/useAPI'

// earned-04: Shadow Mode reads the shadow ledger (/api/v1/shadow-decisions)
// and the trust ledger (/api/v1/trust, for why nothing was recorded). It
// shows each class's counts and its potential minutes, labelled potential,
// and never a heuristic over open issues.
// No concurrent access tests: pure renders over mocked queries.

let responses
vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => {
    const path = (url || '').split('?')[0]
    const data = responses[path]
    if (data === 'loading') {
      return { data: null, loading: true, error: null, refetch: vi.fn() }
    }
    if (data instanceof Error) {
      return { data: null, loading: false, error: data.message, refetch: vi.fn() }
    }
    return { data: data ?? null, loading: false, error: null, refetch: vi.fn() }
  }),
}))

const indexCreate = {
  family: 'tuning', class: 'index_create', total: 5, pending: 0, correct: 4,
  incorrect: 1, neutral: 0, unscored: 0, counted: 3, potential_minutes: 135,
}
const unmodelled = {
  family: 'schema', class: 'unclassified', total: 2, pending: 1, correct: 1,
  incorrect: 0, neutral: 0, unscored: 0, counted: 1, potential_minutes: null,
}

function ledger(summary) {
  return { databases: [{ database: 'prod', summary, decisions: [] }], meaning: 'm' }
}

function trust(enforced, levels) {
  return {
    enforced,
    databases: [{
      database: 'prod',
      rows: levels.map(([cls, level]) => ({ kind: 'self_initiated', class: cls, level })),
    }],
  }
}

beforeEach(() => {
  responses = {
    '/api/v1/shadow-decisions': ledger([indexCreate, unmodelled]),
    '/api/v1/trust': trust(true, [['index_create', 1]]),
  }
  useAPI.mockClear()
})

describe('ShadowModePage', () => {
  it('reads the shadow ledger, never the retired report', () => {
    render(<ShadowModePage database="prod" />)
    const urls = useAPI.mock.calls.map(c => c[0])
    expect(urls).toContain('/api/v1/shadow-decisions?database=prod')
    expect(urls).toContain('/api/v1/trust?database=prod')
    expect(urls.every(u => !/shadow-rep/.test(u))).toBe(true)
  })

  it('reads every database without a database parameter on all', () => {
    render(<ShadowModePage database="all" />)
    const urls = useAPI.mock.calls.map(c => c[0])
    expect(urls).toContain('/api/v1/shadow-decisions')
    expect(urls).toContain('/api/v1/trust')
  })

  it('shows a class counts and potential minutes from the ledger', () => {
    render(<ShadowModePage database="prod" />)
    const row = screen.getByTestId('shadow-class-prod-index_create')
    expect(within(row).getByText('index_create')).toBeInTheDocument()
    expect(within(row).getByTestId('shadow-total')).toHaveTextContent(/^5$/)
    expect(within(row).getByTestId('shadow-pending')).toHaveTextContent(/^0$/)
    expect(within(row).getByTestId('shadow-correct')).toHaveTextContent(/^4$/)
    expect(within(row).getByTestId('shadow-incorrect')).toHaveTextContent(/^1$/)
    expect(within(row).getByTestId('shadow-counted')).toHaveTextContent(/^3$/)
    expect(within(row).getByTestId('shadow-potential'))
      .toHaveTextContent(/^135 min potential$/)
    expect(screen.getByText(/Potential minutes/)).toBeInTheDocument()
    expect(screen.queryByText(/avoided/i)).toBeNull()
  })

  it('reuses the Trust page decision list for each class', () => {
    render(<ShadowModePage database="prod" />)
    const row = screen.getByTestId('shadow-class-prod-index_create')
    expect(within(row).getByRole('button', { name: 'What pg_sage would have done' }))
      .toBeInTheDocument()
  })

  it('says not modelled for a class with no toil model, never 0 or 30', () => {
    render(<ShadowModePage database="prod" />)
    const row = screen.getByTestId('shadow-class-prod-unclassified')
    const cell = within(row).getByTestId('shadow-potential')
    expect(cell).toHaveTextContent(/^not modelled$/)
    expect(cell).not.toHaveTextContent(/\d/)
  })

  it('treats a missing potential like a null one', () => {
    const { potential_minutes: _drop, ...noField } = unmodelled
    responses['/api/v1/shadow-decisions'] = ledger([noField])
    render(<ShadowModePage database="prod" />)
    const row = screen.getByTestId('shadow-class-prod-unclassified')
    expect(within(row).getByTestId('shadow-potential')).toHaveTextContent('not modelled')
  })

  it('names a zero potential as zero when the class is modelled', () => {
    responses['/api/v1/shadow-decisions'] = ledger([{ ...indexCreate, potential_minutes: 0 }])
    render(<ShadowModePage database="prod" />)
    const row = screen.getByTestId('shadow-class-prod-index_create')
    expect(within(row).getByTestId('shadow-potential'))
      .toHaveTextContent(/^0 min potential$/)
  })
})

describe('ShadowModePage empty state', () => {
  beforeEach(() => {
    responses['/api/v1/shadow-decisions'] = ledger([])
  })

  it('says nothing was recorded yet when classes are below L3', () => {
    render(<ShadowModePage database="prod" />)
    const empty = screen.getByTestId('shadow-empty')
    expect(empty).toHaveTextContent(/No shadow decisions yet/)
    expect(empty).toHaveTextContent(/nothing recorded yet/i)
    expect(screen.queryByTestId('shadow-class-prod-index_create')).toBeNull()
  })

  it('names sre.autonomy.enforce when the ledger is not enforced', () => {
    responses['/api/v1/trust'] = trust(false, [['index_create', 1]])
    render(<ShadowModePage database="prod" />)
    const empty = screen.getByTestId('shadow-empty')
    expect(empty).toHaveTextContent(/No shadow decisions yet/)
    expect(empty).toHaveTextContent(/sre\.autonomy\.enforce/)
    expect(empty).not.toHaveTextContent(/nothing recorded yet/i)
  })

  it('says every class is already at L3 when none is below it', () => {
    responses['/api/v1/trust'] = trust(true, [['index_create', 3], ['vacuum', 3]])
    render(<ShadowModePage database="prod" />)
    const empty = screen.getByTestId('shadow-empty')
    expect(empty).toHaveTextContent(/already at L3/)
    expect(empty).not.toHaveTextContent(/sre\.autonomy\.enforce/)
  })

  it('still answers when the trust ledger cannot be read', () => {
    responses['/api/v1/trust'] = new Error('not found (404)')
    render(<ShadowModePage database="prod" />)
    const empty = screen.getByTestId('shadow-empty')
    expect(empty).toHaveTextContent(/No shadow decisions yet/)
    expect(empty).toHaveTextContent(/trust ledger/)
  })

  // The trust API serves levels as labels ("L3"), and the gate shadows by
  // the effective level, so a stored L3 downgraded to L1 is still below it.
  it('reads the trust API level labels when every class is at L3', () => {
    responses['/api/v1/trust'] = trust(true, [['index_create', 'L3'], ['vacuum', 'L3']])
    render(<ShadowModePage database="prod" />)
    expect(screen.getByTestId('shadow-empty')).toHaveTextContent(/already at L3/)
  })

  it('does not call a class at L3 when its effective level is lower', () => {
    responses['/api/v1/trust'] = trust(true, [['index_create', 'L3']])
    responses['/api/v1/trust'].databases[0].rows[0].effective = 'L1'
    render(<ShadowModePage database="prod" />)
    const empty = screen.getByTestId('shadow-empty')
    expect(empty).not.toHaveTextContent(/already at L3/)
    expect(empty).toHaveTextContent(/nothing recorded yet/i)
  })

  it('says it is still checking while the trust ledger loads', () => {
    responses['/api/v1/trust'] = 'loading'
    render(<ShadowModePage database="prod" />)
    const empty = screen.getByTestId('shadow-empty')
    expect(empty).toHaveTextContent(/No shadow decisions yet/)
    expect(empty).not.toHaveTextContent(/could not be read/)
  })

  it('treats a ledger with no databases as empty', () => {
    responses['/api/v1/shadow-decisions'] = { databases: [] }
    render(<ShadowModePage database="all" />)
    expect(screen.getByTestId('shadow-empty')).toHaveTextContent(/No shadow decisions yet/)
  })
})
