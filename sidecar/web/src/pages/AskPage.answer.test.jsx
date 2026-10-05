import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AskPage } from './AskPage'
import {
  investigationAction, makeAnswer, operator, proposalAction, response, stubAsk, viewer,
} from './ask/testkit'

// How one Answer renders: status badge and status messages, statements
// with numbered citation markers, the citations list, "Could not verify",
// the dropped-claims disclosure and the actions section. Ask Sage never
// executes or approves anything, so no approve/execute control may appear.

afterEach(() => vi.unstubAllGlobals())

async function renderAnswer(answer, user = operator) {
  const mock = stubAsk({ ask: () => response(200, answer) })
  const view = render(<AskPage database="orders" user={user} />)
  fireEvent.change(screen.getByTestId('ask-input'), { target: { value: 'q' } })
  fireEvent.click(screen.getByTestId('ask-submit'))
  const node = await screen.findByTestId(`ask-answer-${answer.id}`)
  return { mock, node, container: view.container }
}

const noApproveControls = () => {
  const buttons = screen.queryAllByRole('button')
  expect(buttons.filter(b => /approve|execute|apply|run sql/i.test(b.textContent
    + (b.getAttribute('aria-label') || '')))).toEqual([])
}

describe('answer body', () => {
  it('shows the question, the status and the statements, not the server text', async () => {
    const { node } = await renderAnswer(makeAnswer())
    expect(within(node).getByTestId('ask-question')).toHaveTextContent('Why is orders slow?')
    expect(within(node).getByTestId('ask-status')).toHaveTextContent('Answered')
    const statements = within(node).getAllByTestId('ask-statement')
    expect(statements).toHaveLength(2)
    expect(statements[0]).toHaveTextContent('Sequential scans on public.orders dominate.')
    expect(statements[1]).toHaveTextContent('An index on customer_id was proposed.')
    expect(node).not.toHaveTextContent('SERVER-RENDERED-TEXT')
    expect(within(node).queryByTestId('ask-status-message')).toBeNull()
  })

  it('numbers citation markers and links them to the evidence page', async () => {
    const { node } = await renderAnswer(makeAnswer())
    const [first, second] = within(node).getAllByTestId('ask-statement')
    expect(within(first).getByRole('link', { name: '[1]' })).toHaveAttribute('href', '#/cases')
    // queries evidence has no UI page: the marker is plain text.
    const plain = within(first).getByText('[2]')
    expect(plain.closest('a')).toBeNull()
    expect(within(second).getByRole('link', { name: '[3]' }))
      .toHaveAttribute('href', '#/actions')
  })

  it('skips markers for citation ids missing from the citations list', async () => {
    const { node } = await renderAnswer(makeAnswer({
      statements: [{ text: 'Ghost claim.', citations: ['finding:999', 'finding:17'] }],
    }))
    const statement = within(node).getByTestId('ask-statement')
    expect(statement).toHaveTextContent('Ghost claim.')
    expect(statement).toHaveTextContent('[1]')
    expect(statement).not.toHaveTextContent('finding:999')
    expect(within(statement).getAllByRole('link')).toHaveLength(1)
  })

  it('lists citations with label, kind, a 12 character digest and a link', async () => {
    const { node } = await renderAnswer(makeAnswer())
    const items = within(within(node).getByTestId('ask-citations'))
      .getAllByTestId('ask-citation')
    expect(items).toHaveLength(3)
    expect(items[0]).toHaveTextContent('[1]')
    expect(items[0]).toHaveTextContent('Finding #17: seq scans')
    expect(items[0]).toHaveTextContent('finding')
    expect(items[0]).toHaveTextContent('a1b2c3d4e5f6')
    expect(items[0]).not.toHaveTextContent('a1b2c3d4e5f6a')
    expect(within(items[0]).getByRole('link')).toHaveAttribute('href', '#/cases')
    expect(items[1]).toHaveTextContent('Top queries by time')
    expect(items[1]).toHaveTextContent('queries')
    expect(items[1]).toHaveTextContent('0123456789ab')
    expect(within(items[1]).queryByRole('link')).toBeNull()
    expect(within(items[2]).getByRole('link')).toHaveAttribute('href', '#/actions')
  })

  it('lists what Sage could not verify', async () => {
    const { node } = await renderAnswer(makeAnswer())
    const section = within(node).getByTestId('ask-not-verified')
    expect(section).toHaveTextContent(/could not verify/i)
    expect(section).toHaveTextContent('Whether the application retries failed requests')
  })

  it('collapses dropped claims and marks them not verified', async () => {
    const { node } = await renderAnswer(makeAnswer())
    const dropped = within(node).getByTestId('ask-dropped')
    expect(dropped.tagName).toBe('DETAILS')
    expect(dropped).not.toHaveAttribute('open')
    expect(dropped.querySelector('summary')).toHaveTextContent('2 dropped')
    const items = within(dropped).getAllByTestId('ask-dropped-item')
    expect(items).toHaveLength(2)
    expect(items[0]).toHaveTextContent('Autovacuum is disabled')
    expect(items[0]).toHaveTextContent('no citation')
    expect(items[0]).toHaveTextContent(/not verified/i)
    expect(items[1]).toHaveTextContent('cited evidence does not mention it')
  })

  it('omits empty sections and tolerates missing fields', async () => {
    const { node } = await renderAnswer({ id: 5, conversation_id: 'c-1',
      question: 'bare', status: 'not_observed', statements: null })
    expect(within(node).getByTestId('ask-status')).toHaveTextContent('Not observed')
    expect(within(node).queryAllByTestId('ask-statement')).toHaveLength(0)
    for (const id of ['ask-citations', 'ask-not-verified', 'ask-dropped', 'ask-actions']) {
      expect(within(node).queryByTestId(id)).toBeNull()
    }
  })
})

describe('status messages', () => {
  it.each([
    ['not_observed', 'Not observed'], ['budget_exhausted', 'Budget exhausted'],
    ['llm_unavailable', 'LLM unavailable'], ['incomplete', 'Incomplete'],
  ])('badges %s as "%s"', async (status, label) => {
    const { node } = await renderAnswer(makeAnswer({ status }))
    expect(within(node).getByTestId('ask-status')).toHaveTextContent(label)
  })

  it('explains an exhausted budget', async () => {
    const { node } = await renderAnswer(makeAnswer({ status: 'budget_exhausted',
      statements: [], citations: [] }))
    expect(within(node).getByTestId('ask-status-message')).toHaveTextContent(/budget/i)
  })

  it('says an LLM must be configured when none is available', async () => {
    const { node } = await renderAnswer(makeAnswer({ status: 'llm_unavailable',
      statements: [], citations: [] }))
    expect(within(node).getByTestId('ask-status-message'))
      .toHaveTextContent(/LLM must be configured/i)
  })

  it('says an incomplete answer was cut short and why', async () => {
    const { node } = await renderAnswer(makeAnswer({ status: 'incomplete',
      stop: 'max_tokens' }))
    const msg = within(node).getByTestId('ask-status-message')
    expect(msg).toHaveTextContent(/cut short/i)
    expect(msg).toHaveTextContent('max_tokens')
  })
})

describe('actions', () => {
  it('shows a queued proposal with its SQL and sends approval to Actions', async () => {
    const { node } = await renderAnswer(makeAnswer({ actions: [proposalAction()] }))
    const action = within(within(node).getByTestId('ask-actions'))
      .getByTestId('ask-action')
    expect(action).toHaveTextContent(/queued for approval/i)
    expect(action).toHaveTextContent(
      'CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders (customer_id)')
    expect(action).toHaveTextContent('DROP INDEX CONCURRENTLY idx_orders_customer')
    expect(action).toHaveTextContent('reversible')
    expect(action).toHaveTextContent('needs_approval')
    const link = within(action).getByRole('link', { name: /actions page/i })
    expect(link).toHaveAttribute('href', '#/actions')
    noApproveControls()
  })

  it('shows an already pending proposal with a link to Actions', async () => {
    const { node } = await renderAnswer(makeAnswer({
      actions: [proposalAction({ status: 'already_pending' })] }))
    const action = within(node).getByTestId('ask-action')
    expect(action).toHaveTextContent(/already pending/i)
    expect(within(action).getByRole('link', { name: /actions page/i }))
      .toHaveAttribute('href', '#/actions')
  })

  it.each(['blocked', 'refused', 'failed'])('shows the reason a proposal was %s',
    async status => {
      const reason = `policy says no (${status})`
      const { node } = await renderAnswer(makeAnswer({
        actions: [proposalAction({ status, reason })] }))
      const action = within(node).getByTestId('ask-action')
      expect(action).toHaveTextContent(new RegExp(status, 'i'))
      expect(action).toHaveTextContent(reason)
      expect(action).not.toHaveTextContent(/queued for approval/i)
      noApproveControls()
    })

  it('shows an opened investigation with a link to Cases', async () => {
    const { node } = await renderAnswer(makeAnswer({ actions: [investigationAction()] }))
    const action = within(node).getByTestId('ask-action')
    expect(action).toHaveTextContent(/opened/i)
    expect(within(action).getByRole('link')).toHaveAttribute('href', '#/cases')
  })

  it('shows a joined investigation', async () => {
    const { node } = await renderAnswer(makeAnswer({
      actions: [investigationAction({ status: 'joined' })] }))
    expect(within(node).getByTestId('ask-action')).toHaveTextContent(/joined/i)
  })

  it('renders no approve or execute control for any action outcome', async () => {
    const statuses = ['queued', 'already_pending', 'blocked', 'refused', 'failed']
    const actions = [
      ...statuses.map((status, i) => proposalAction({ id: i + 1, status, reason: 'r' })),
      investigationAction(), investigationAction({ id: 'inv-6', status: 'joined' }),
    ]
    const { node } = await renderAnswer(makeAnswer({ actions }), viewer)
    expect(within(node).getAllByTestId('ask-action')).toHaveLength(7)
    noApproveControls()
  })
})

describe('escaping', () => {
  it('renders HTML from the answer as text, never as markup', async () => {
    const payload = '<img src=x onerror=alert(1)>'
    const alert = vi.spyOn(window, 'alert').mockImplementation(() => {})
    const { node, container } = await renderAnswer(makeAnswer({
      question: `q ${payload}`,
      statements: [{ text: `Statement ${payload}`, citations: ['finding:17'] }],
      not_verified: [`nv ${payload}`],
      dropped: [{ text: `dropped ${payload}`, reason: `reason ${payload}` }],
      citations: [{ id: 'finding:17', kind: 'finding', ref: '17',
        label: `label ${payload}`, digest: '<script>x</script>', api_path: '' }],
      actions: [proposalAction({ status: 'blocked', reason: `why ${payload}`,
        sql: `SELECT '${payload}'` })],
    }))
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('script')).toBeNull()
    expect(within(node).getByTestId('ask-statement'))
      .toHaveTextContent(`Statement ${payload}`)
    expect(node).toHaveTextContent(`label ${payload}`)
    expect(node).toHaveTextContent(`why ${payload}`)
    expect(node).toHaveTextContent(`nv ${payload}`)
    expect(alert).not.toHaveBeenCalled()
  })

  it('never links a citation kind that is not a known page', async () => {
    const { node } = await renderAnswer(makeAnswer({
      statements: [{ text: 'x', citations: ['evil'] }],
      citations: [{ id: 'evil', kind: 'javascript:alert(1)', ref: '',
        label: 'evil', digest: 'd', api_path: 'javascript:alert(1)' }],
    }))
    expect(within(node).queryAllByRole('link')).toHaveLength(0)
  })
})
