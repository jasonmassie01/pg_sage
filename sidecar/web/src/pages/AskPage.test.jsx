import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AskPage } from './AskPage'
import {
  BASE, admin, budgetBody, getsTo, makeAnswer, operator, postBodies, posts,
  response, stubAsk, viewer,
} from './ask/testkit'

// AskPage: the composer, conversation threading, the conversation list,
// the budget meter, the fleet-mode guard and the viewer note. fetch is
// stubbed per route so the exact requests the page sends are asserted.

afterEach(() => vi.unstubAllGlobals())

const input = () => screen.getByTestId('ask-input')
const submit = () => screen.getByTestId('ask-submit')

function type(text) {
  fireEvent.change(input(), { target: { value: text } })
}

async function ask(text) {
  type(text)
  fireEvent.click(submit())
}

describe('AskPage database guard', () => {
  it.each([['all'], [''], [undefined], [null]])(
    'asks to pick one database for %j and makes no API calls', db => {
      const mock = stubAsk()
      render(<AskPage database={db} user={operator} />)
      expect(screen.getByTestId('ask-pick-database')).toHaveTextContent(/one database/i)
      expect(screen.queryByTestId('ask-input')).toBeNull()
      expect(mock).not.toHaveBeenCalled()
    })

  it('URL-encodes the database name in every request', async () => {
    const mock = stubAsk({ ask: () => response(200, makeAnswer({ database: 'my db/x' })) })
    render(<AskPage database="my db/x" user={operator} />)
    await ask('hi')
    await screen.findByTestId('ask-answer-41')
    const urls = mock.mock.calls.map(([url]) => String(url))
    expect(urls).toContain('/api/v1/databases/my%20db%2Fx/ask')
    expect(urls).toContain('/api/v1/databases/my%20db%2Fx/ask/budget')
    expect(urls).toContain('/api/v1/databases/my%20db%2Fx/ask/conversations')
    expect(urls.every(u => !u.includes('my db/x'))).toBe(true)
  })
})

describe('AskPage composer', () => {
  it('disables Ask while the question is empty or only whitespace', async () => {
    stubAsk()
    render(<AskPage database="orders" user={operator} />)
    expect(await screen.findByTestId('ask-page')).toBeInTheDocument()
    expect(submit()).toHaveTextContent('Ask')
    expect(submit()).toBeDisabled()
    type('   \n  ')
    expect(submit()).toBeDisabled()
    type('why?')
    expect(submit()).toBeEnabled()
  })

  it('posts the trimmed question, appends the answer and clears the input', async () => {
    const mock = stubAsk()
    render(<AskPage database="orders" user={operator} />)
    await ask('  Why is orders slow?  ')
    const answer = await screen.findByTestId('ask-answer-41')
    const [[url, init]] = posts(mock)
    expect(url).toBe(BASE)
    expect(init.credentials).toBe('include')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ question: 'Why is orders slow?' })
    expect(within(answer).getByTestId('ask-question')).toHaveTextContent(
      'Why is orders slow?')
    expect(input()).toHaveValue('')
  })

  it('submits on Ctrl+Enter and Cmd+Enter but not on a plain Enter', async () => {
    const mock = stubAsk()
    render(<AskPage database="orders" user={operator} />)
    type('first')
    fireEvent.keyDown(input(), { key: 'Enter' })
    expect(posts(mock)).toHaveLength(0)
    fireEvent.keyDown(input(), { key: 'Enter', ctrlKey: true })
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    await screen.findByTestId('ask-answer-41')
    type('second')
    fireEvent.keyDown(input(), { key: 'Enter', metaKey: true })
    await waitFor(() => expect(posts(mock)).toHaveLength(2))
    expect(postBodies(mock).map(b => b.question)).toEqual(['first', 'second'])
  })

  it('does not submit an empty question with Ctrl+Enter', async () => {
    const mock = stubAsk()
    render(<AskPage database="orders" user={operator} />)
    fireEvent.keyDown(input(), { key: 'Enter', ctrlKey: true })
    type('  ')
    fireEvent.keyDown(input(), { key: 'Enter', ctrlKey: true })
    await new Promise(r => setTimeout(r, 0))
    expect(posts(mock)).toHaveLength(0)
  })

  it('disables Ask while a request is in flight and sends only once', async () => {
    let release
    const pending = new Promise(r => { release = r })
    const mock = stubAsk({ ask: () => pending })
    render(<AskPage database="orders" user={operator} />)
    await ask('slow?')
    await waitFor(() => expect(submit()).toBeDisabled())
    type('again')
    expect(submit()).toBeDisabled()
    fireEvent.click(submit())
    fireEvent.keyDown(input(), { key: 'Enter', ctrlKey: true })
    expect(posts(mock)).toHaveLength(1)
    release(response(200, makeAnswer()))
    await screen.findByTestId('ask-answer-41')
    await waitFor(() => expect(submit()).toBeEnabled())
  })

  it('enforces the 2000 character limit and shows the count', async () => {
    const mock = stubAsk()
    render(<AskPage database="orders" user={operator} />)
    expect(input()).toHaveAttribute('maxLength', '2000')
    expect(screen.getByTestId('ask-char-count')).toHaveTextContent('0 / 2000')
    type('x'.repeat(2001))
    expect(input().value).toHaveLength(2000)
    expect(screen.getByTestId('ask-char-count')).toHaveTextContent('2000 / 2000')
    expect(submit()).toBeEnabled()
    fireEvent.click(submit())
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(postBodies(mock)[0].question).toHaveLength(2000)
  })

  it('counts 1999 characters below the limit', () => {
    stubAsk()
    render(<AskPage database="orders" user={operator} />)
    type('y'.repeat(1999))
    expect(screen.getByTestId('ask-char-count')).toHaveTextContent('1999 / 2000')
  })
})

describe('AskPage conversations', () => {
  it('threads follow-up questions into the current conversation', async () => {
    let n = 0
    const mock = stubAsk({ ask: () => {
      n += 1
      return response(200, makeAnswer({ id: 40 + n, question: `q${n}` }))
    } })
    render(<AskPage database="orders" user={operator} />)
    await ask('q1')
    await screen.findByTestId('ask-answer-41')
    await ask('q2')
    await screen.findByTestId('ask-answer-42')
    expect(postBodies(mock)).toEqual([
      { question: 'q1' }, { question: 'q2', conversation_id: 'c-1' },
    ])
    const questions = screen.getAllByTestId('ask-question').map(q => q.textContent)
    expect(questions).toEqual(['q1', 'q2'])
  })

  it('starts a new conversation that clears the thread and the id', async () => {
    const mock = stubAsk()
    render(<AskPage database="orders" user={operator} />)
    await ask('q1')
    await screen.findByTestId('ask-answer-41')
    fireEvent.click(screen.getByTestId('ask-new-conversation'))
    expect(screen.queryByTestId('ask-answer-41')).toBeNull()
    await ask('fresh')
    await waitFor(() => expect(posts(mock)).toHaveLength(2))
    expect(postBodies(mock)[1]).toEqual({ question: 'fresh' })
  })

  it('lists the caller conversations and loads one into the thread', async () => {
    const list = [
      { id: 'c-9', title: 'Bloat on events', messages: 4,
        created_at: '2026-10-04T08:00:00Z', updated_at: '2026-10-04T09:00:00Z' },
      { id: 'c-8', title: 'Older question', messages: 2,
        created_at: '2026-10-03T08:00:00Z', updated_at: '2026-10-03T09:00:00Z' },
    ]
    const mock = stubAsk({
      conversations: () => response(200, { conversations: list }),
      conversation: () => response(200, { conversation: list[0], answers: [
        makeAnswer({ id: 1, conversation_id: 'c-9', question: 'first' }),
        makeAnswer({ id: 2, conversation_id: 'c-9', question: 'second' }),
      ] }),
    })
    render(<AskPage database="orders" user={operator} />)
    const item = await screen.findByTestId('ask-conversation-c-9')
    expect(item).toHaveTextContent('Bloat on events')
    expect(screen.getByTestId('ask-conversation-c-8')).toHaveTextContent('Older question')
    const order = within(screen.getByTestId('ask-conversations'))
      .getAllByTestId(/^ask-conversation-c-/).map(e => e.dataset.testid)
    expect(order).toEqual(['ask-conversation-c-9', 'ask-conversation-c-8'])
    fireEvent.click(item)
    await screen.findByTestId('ask-answer-2')
    expect(getsTo(mock, `${BASE}/conversations/c-9`)).toHaveLength(1)
    expect(screen.getAllByTestId('ask-question').map(q => q.textContent))
      .toEqual(['first', 'second'])
    expect(screen.getByTestId('ask-conversation-c-9')).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByTestId('ask-conversation-c-8')).toHaveAttribute('aria-pressed', 'false')
    await ask('third')
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(postBodies(mock)[0]).toEqual({ question: 'third', conversation_id: 'c-9' })
  })

  it('URL-encodes the conversation id when loading it', async () => {
    const conv = { id: 'c/1', title: 'odd id', messages: 0 }
    const mock = stubAsk({
      conversations: () => response(200, { conversations: [conv] }),
      conversation: () => response(200, { conversation: conv, answers: [] }),
    })
    render(<AskPage database="orders" user={operator} />)
    fireEvent.click(await screen.findByTestId('ask-conversation-c/1'))
    await waitFor(() => expect(getsTo(mock, `${BASE}/conversations/c%2F1`)).toHaveLength(1))
  })

  it('shows an empty conversation list', async () => {
    stubAsk({ conversations: () => response(200, {}) })
    render(<AskPage database="orders" user={operator} />)
    expect(await screen.findByTestId('ask-conversations-empty')).toBeInTheDocument()
  })

  it('refreshes the conversation list after an answer', async () => {
    const mock = stubAsk()
    render(<AskPage database="orders" user={operator} />)
    await waitFor(() => expect(getsTo(mock, `${BASE}/conversations`)).toHaveLength(1))
    await ask('q1')
    await screen.findByTestId('ask-answer-41')
    await waitFor(() => expect(getsTo(mock, `${BASE}/conversations`)).toHaveLength(2))
  })
})

describe('AskPage budget meter', () => {
  it('shows user and database usage and refreshes after each answer', async () => {
    let used = 3
    const mock = stubAsk({ budget: () => response(200, budgetBody({
      user_used: used, database_used: used + 7 })) })
    render(<AskPage database="orders" user={operator} />)
    expect(await screen.findByTestId('ask-budget-user')).toHaveTextContent('3 / 20')
    expect(screen.getByTestId('ask-budget-database')).toHaveTextContent('10 / 100')
    used = 4
    await ask('q1')
    await screen.findByTestId('ask-answer-41')
    await waitFor(() => expect(screen.getByTestId('ask-budget-user'))
      .toHaveTextContent('4 / 20'))
    expect(screen.getByTestId('ask-budget-database')).toHaveTextContent('11 / 100')
    expect(getsTo(mock, `${BASE}/budget`).length).toBeGreaterThanOrEqual(2)
  })

  it('keeps the composer usable when the budget cannot be loaded', async () => {
    const mock = stubAsk({ budget: () => response(500, { error: 'boom' }) })
    render(<AskPage database="orders" user={operator} />)
    await waitFor(() => expect(getsTo(mock, `${BASE}/budget`)).toHaveLength(1))
    expect(screen.queryByTestId('ask-budget-user')).toBeNull()
    type('still works?')
    expect(submit()).toBeEnabled()
  })

  it('shows zero usage', async () => {
    stubAsk({ budget: () => response(200, budgetBody({ user_used: 0, database_used: 0 })) })
    render(<AskPage database="orders" user={operator} />)
    expect(await screen.findByTestId('ask-budget-user')).toHaveTextContent('0 / 20')
    expect(screen.getByTestId('ask-budget-database')).toHaveTextContent('0 / 100')
  })
})

describe('AskPage roles', () => {
  it('tells viewers that proposals need the operator role', async () => {
    stubAsk()
    render(<AskPage database="orders" user={viewer} />)
    expect(await screen.findByTestId('ask-viewer-note')).toHaveTextContent(/operator/i)
    // Viewers can still ask: the server decides what they may propose.
    type('why?')
    expect(submit()).toBeEnabled()
  })

  it.each([['operator', operator], ['admin', admin]])(
    'shows no viewer note to an %s', async (_, user) => {
      stubAsk()
      render(<AskPage database="orders" user={user} />)
      await screen.findByTestId('ask-page')
      expect(screen.queryByTestId('ask-viewer-note')).toBeNull()
    })
})
