import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AskPage } from './AskPage'
import {
  makeAnswer, operator, postBodies, posts, response, stubAsk,
} from './ask/testkit'

// Error handling: each server error code is distinguishable on screen,
// a failed request leaves the thread and the typed question intact, and a
// network failure offers a retry that resends the same question.

afterEach(() => vi.unstubAllGlobals())

function askQuestion(text) {
  fireEvent.change(screen.getByTestId('ask-input'), { target: { value: text } })
  fireEvent.click(screen.getByTestId('ask-submit'))
}

describe('AskPage errors', () => {
  it('explains a disabled Ask Sage with a configuration hint', async () => {
    stubAsk({ ask: () => response(503, { error: 'ask is disabled', code: 'ask_disabled' }) })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('hello')
    const notice = await screen.findByTestId('ask-disabled')
    expect(notice).toHaveTextContent('Ask Sage is disabled')
    expect(screen.getByTestId('ask-disabled-hint')).toHaveTextContent('ask.enabled')
    expect(screen.queryByTestId('ask-error')).toBeNull()
  })

  it('shows the disabled notice up front when the budget says so', async () => {
    const mock = stubAsk({ budget: () => response(503,
      { error: 'ask is disabled', code: 'ask_disabled' }) })
    render(<AskPage database="orders" user={operator} />)
    expect(await screen.findByTestId('ask-disabled')).toHaveTextContent(
      'Ask Sage is disabled')
    expect(posts(mock)).toHaveLength(0)
  })

  it('shows the server message for an unavailable Ask Sage', async () => {
    stubAsk({ ask: () => response(503,
      { error: 'the LLM pool is exhausted', code: 'ask_unavailable' }) })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('hello')
    expect(await screen.findByTestId('ask-error')).toHaveTextContent(
      'the LLM pool is exhausted')
    expect(screen.queryByTestId('ask-disabled')).toBeNull()
  })

  it('shows the server error for an invalid request and keeps the question', async () => {
    stubAsk({ ask: () => response(400,
      { error: 'question must not be empty', code: 'invalid_request' }) })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('bad one')
    expect(await screen.findByTestId('ask-error')).toHaveTextContent(
      'question must not be empty')
    expect(screen.getByTestId('ask-input')).toHaveValue('bad one')
    expect(screen.queryAllByTestId(/^ask-answer-/)).toHaveLength(0)
    expect(screen.getByTestId('ask-submit')).toBeEnabled()
  })

  it('shows not found for an unknown conversation', async () => {
    stubAsk({ ask: () => response(404,
      { error: 'conversation not found', code: 'not_found' }) })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('hello')
    expect(await screen.findByTestId('ask-error')).toHaveTextContent(
      'conversation not found')
  })

  it('reports an expired session on 401', async () => {
    stubAsk({ ask: () => response(401, { error: 'unauthorized' }) })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('hello')
    expect(await screen.findByTestId('ask-error')).toHaveTextContent(/session expired/i)
  })

  it('falls back to the HTTP status when the error body is not JSON', async () => {
    stubAsk({ ask: () => ({ ok: false, status: 502, statusText: 'Bad Gateway',
      json: async () => { throw new SyntaxError('Unexpected token <') } }) })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('hello')
    expect(await screen.findByTestId('ask-error')).toHaveTextContent('502')
  })

  it('keeps earlier answers when a later question fails', async () => {
    let n = 0
    stubAsk({ ask: () => {
      n += 1
      return n === 1 ? response(200, makeAnswer())
        : response(400, { error: 'too long', code: 'invalid_request' })
    } })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('q1')
    await screen.findByTestId('ask-answer-41')
    askQuestion('q2')
    await screen.findByTestId('ask-error')
    expect(screen.getByTestId('ask-answer-41')).toBeInTheDocument()
  })

  it('offers a retry after a network failure that resends the question', async () => {
    let n = 0
    const mock = stubAsk({ ask: () => {
      n += 1
      if (n === 1) throw new TypeError('Failed to fetch')
      return response(200, makeAnswer())
    } })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('flaky?')
    const retry = await screen.findByRole('button', { name: 'Retry' })
    expect(screen.getByText(/failed to fetch/i)).toBeInTheDocument()
    fireEvent.click(retry)
    await screen.findByTestId('ask-answer-41')
    expect(postBodies(mock)).toEqual([{ question: 'flaky?' }, { question: 'flaky?' }])
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
    expect(screen.queryByText(/failed to fetch/i)).toBeNull()
  })

  it('clears the error once a question succeeds', async () => {
    let n = 0
    stubAsk({ ask: () => {
      n += 1
      return n === 1 ? response(400, { error: 'nope', code: 'invalid_request' })
        : response(200, makeAnswer())
    } })
    render(<AskPage database="orders" user={operator} />)
    askQuestion('q1')
    await screen.findByTestId('ask-error')
    askQuestion('q1 again')
    await screen.findByTestId('ask-answer-41')
    expect(screen.queryByTestId('ask-error')).toBeNull()
  })

  it('reports a conversation that cannot be loaded', async () => {
    const conv = { id: 'c-3', title: 'gone', messages: 1 }
    stubAsk({
      conversations: () => response(200, { conversations: [conv] }),
      conversation: () => response(404, { error: 'conversation not found',
        code: 'not_found' }),
    })
    render(<AskPage database="orders" user={operator} />)
    fireEvent.click(await screen.findByTestId('ask-conversation-c-3'))
    await waitFor(() => expect(screen.getByTestId('ask-error'))
      .toHaveTextContent('conversation not found'))
    expect(screen.queryAllByTestId(/^ask-answer-/)).toHaveLength(0)
  })

  it('reports a conversation list that cannot be loaded', async () => {
    stubAsk({ conversations: () => { throw new TypeError('Failed to fetch') } })
    render(<AskPage database="orders" user={operator} />)
    expect(await screen.findByTestId('ask-conversations-error'))
      .toHaveTextContent(/failed to fetch/i)
    expect(screen.getByTestId('ask-input')).toBeInTheDocument()
  })
})
