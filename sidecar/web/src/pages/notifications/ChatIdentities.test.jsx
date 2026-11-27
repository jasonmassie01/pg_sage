import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChatIdentities } from './ChatIdentities'
import { stubFetch } from '../../test/fetchStub'
import { answerConfirm } from '../../test/confirmDialog'

// alerting-10: an admin links a Slack or Telegram user to a pg_sage user
// over /api/v1/chatops/identities, so that user can approve from chat.
// Anyone else gets a 403 and sees no panel. dashboard-10b: unlink asks in
// the shared ConfirmDialog, not window.confirm.
// Concurrency: the in-flight double click below; no other shared state.

afterEach(() => vi.unstubAllGlobals())

const IDS = '/api/v1/chatops/identities'
const USERS = '/api/v1/users'

const linked = [
  { id: 3, provider: 'slack', team_id: 'T1', external_user_id: 'U1', user_id: 7,
    user_email: 'ops@example.com', created_at: '2030-01-01T00:00:00Z' },
  { id: 4, provider: 'telegram', team_id: '', external_user_id: '4242', user_id: 8,
    user_email: 'dba@example.com', created_at: '2030-01-01T00:00:00Z' },
]
const users = [
  { id: 7, email: 'ops@example.com', role: 'operator' },
  { id: 8, email: 'dba@example.com', role: 'admin' },
]

describe('ChatIdentities', () => {
  it('lists linked chat users with their pg_sage account', async () => {
    stubFetch({ [IDS]: { json: { items: linked } }, [USERS]: { json: { users } } })
    render(<ChatIdentities />)
    const rows = await screen.findAllByTestId('chat-identity-row')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('slack')
    expect(rows[0]).toHaveTextContent('T1')
    expect(rows[0]).toHaveTextContent('U1')
    expect(rows[0]).toHaveTextContent('ops@example.com')
    expect(rows[1]).toHaveTextContent('4242')
  })

  it('links a Slack user with team, external id and pg_sage user', async () => {
    const fetchFn = stubFetch({
      [`GET ${IDS}`]: [{ json: { items: [] } }, { json: { items: linked.slice(0, 1) } }],
      [`POST ${IDS}`]: { status: 201, json: linked[0] },
      [USERS]: { json: { users } },
    })
    render(<ChatIdentities />)
    await screen.findByText('No chat users linked yet.')
    await screen.findByRole('option', { name: /ops@example.com/ })
    fireEvent.change(screen.getByTestId('chat-identity-team-id'), { target: { value: 'T1' } })
    fireEvent.change(screen.getByTestId('chat-identity-external-user-id'),
      { target: { value: 'U1' } })
    fireEvent.change(screen.getByTestId('chat-identity-user'), { target: { value: '7' } })
    fireEvent.click(screen.getByTestId('chat-identity-link'))

    expect(await screen.findByTestId('chat-identity-row')).toHaveTextContent('U1')
    const post = fetchFn.mock.calls.find(([, init]) => init?.method === 'POST')
    expect(post[0]).toBe(IDS)
    expect(JSON.parse(post[1].body)).toEqual({
      provider: 'slack', team_id: 'T1', external_user_id: 'U1', user_id: 7,
    })
  })

  it('links a Telegram user without a team id', async () => {
    const fetchFn = stubFetch({
      [`GET ${IDS}`]: { json: { items: [] } },
      [`POST ${IDS}`]: { status: 201, json: linked[1] },
      [USERS]: { json: { users } },
    })
    render(<ChatIdentities />)
    await screen.findByRole('option', { name: /dba@example.com/ })
    fireEvent.change(screen.getByTestId('chat-identity-provider'),
      { target: { value: 'telegram' } })
    expect(screen.queryByTestId('chat-identity-team-id')).toBeNull()
    fireEvent.change(screen.getByTestId('chat-identity-external-user-id'),
      { target: { value: '4242' } })
    fireEvent.change(screen.getByTestId('chat-identity-user'), { target: { value: '8' } })
    fireEvent.click(screen.getByTestId('chat-identity-link'))
    await waitFor(() => expect(fetchFn.mock.calls.some(([, i]) => i?.method === 'POST'))
      .toBe(true))
    const post = fetchFn.mock.calls.find(([, init]) => init?.method === 'POST')
    expect(JSON.parse(post[1].body)).toEqual({
      provider: 'telegram', external_user_id: '4242', user_id: 8,
    })
  })

  it('shows the API message when a link is refused', async () => {
    const msg = 'chatops: invalid identity: a Slack mapping needs the workspace team_id'
    stubFetch({
      [`GET ${IDS}`]: { json: { items: [] } },
      [`POST ${IDS}`]: { status: 400, json: { error: msg, code: 'invalid_request' } },
      [USERS]: { json: { users } },
    })
    render(<ChatIdentities />)
    await screen.findByRole('option', { name: /ops@example.com/ })
    fireEvent.change(screen.getByTestId('chat-identity-external-user-id'),
      { target: { value: 'U1' } })
    fireEvent.change(screen.getByTestId('chat-identity-user'), { target: { value: '7' } })
    fireEvent.click(screen.getByTestId('chat-identity-link'))
    expect(await screen.findByRole('alert')).toHaveTextContent(msg)
  })

  it('unlinks an identity and reloads the list', async () => {
    const fetchFn = stubFetch({
      [`GET ${IDS}`]: [{ json: { items: linked } }, { json: { items: linked.slice(1) } }],
      [`DELETE ${IDS}/3`]: { json: { ok: true, id: 3 } },
      [USERS]: { json: { users } },
    })
    render(<ChatIdentities />)
    const rows = await screen.findAllByTestId('chat-identity-row')
    fireEvent.click(within(rows[0]).getByTestId('chat-identity-unlink'))
    await answerConfirm()
    await waitFor(() => expect(screen.getAllByTestId('chat-identity-row')).toHaveLength(1))
    expect(fetchFn.mock.calls.some(([u, i]) => u === `${IDS}/3` && i?.method === 'DELETE'))
      .toBe(true)
  })

  it('renders nothing for a non-admin (403) and shows no error', async () => {
    stubFetch({
      [IDS]: { status: 403, json: { error: 'admin role required' } },
      [USERS]: { status: 403, json: { error: 'admin role required' } },
    })
    const { container } = render(<ChatIdentities />)
    await waitFor(() => expect(container).toBeEmptyDOMElement())
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('shows a short error, never a stack trace, when the list fails', async () => {
    stubFetch({
      [IDS]: { status: 500, text: 'Error: boom\n    at handler (server.go:1)' },
      [USERS]: { json: { users } },
    })
    render(<ChatIdentities />)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Failed to load chat identities (500)')
    expect(alert).not.toHaveTextContent('server.go')
  })

  it('falls back to a numeric user id when the user list is unavailable', async () => {
    const fetchFn = stubFetch({
      [`GET ${IDS}`]: { json: { items: [] } },
      [`POST ${IDS}`]: { status: 201, json: linked[0] },
      [USERS]: { status: 500, text: 'oops' },
    })
    render(<ChatIdentities />)
    await screen.findByText('No chat users linked yet.')
    const user = await screen.findByTestId('chat-identity-user')
    await waitFor(() => expect(user.tagName).toBe('INPUT'))
    fireEvent.change(screen.getByTestId('chat-identity-team-id'), { target: { value: 'T1' } })
    fireEvent.change(screen.getByTestId('chat-identity-external-user-id'),
      { target: { value: 'U1' } })
    fireEvent.change(screen.getByTestId('chat-identity-user'), { target: { value: '7' } })
    fireEvent.click(screen.getByTestId('chat-identity-link'))
    await waitFor(() => expect(fetchFn.mock.calls.some(([, i]) => i?.method === 'POST'))
      .toBe(true))
    const post = fetchFn.mock.calls.find(([, init]) => init?.method === 'POST')
    expect(JSON.parse(post[1].body).user_id).toBe(7)
  })
})

describe('ChatIdentities unlink dialog (dashboard-10b)', () => {
  const deletes = (fetchFn, id = 3) => fetchFn.mock.calls
    .filter(([u, i]) => u === `${IDS}/${id}` && i?.method === 'DELETE')

  async function openUnlink(routes, row = 0) {
    const fetchFn = stubFetch({ [`GET ${IDS}`]: { json: { items: linked } },
      [USERS]: { json: { users } }, ...routes })
    render(<ChatIdentities />)
    const rows = await screen.findAllByTestId('chat-identity-row')
    fireEvent.click(within(rows[row]).getByTestId('chat-identity-unlink'))
    return { fetchFn, dialog: await screen.findByRole('dialog') }
  }

  it('names the chat user and sends exactly the recorded request', async () => {
    const { fetchFn, dialog } = await openUnlink({
      [`DELETE ${IDS}/3`]: { json: { ok: true, id: 3 } },
    })
    expect(within(dialog).getByRole('heading')).toHaveTextContent('Unlink chat user U1')
    expect(dialog).toHaveTextContent('They can no longer approve from chat.')
    expect(within(dialog).getByTestId('confirm-dialog-confirm')).toHaveTextContent('Unlink')
    expect(within(dialog).queryByTestId('confirm-dialog-risk')).toBeNull()
    expect(deletes(fetchFn)).toHaveLength(0)
    await answerConfirm()
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(deletes(fetchFn)).toEqual([[`${IDS}/3`, { method: 'DELETE', credentials: 'include' }]])
  })

  it('shows a chat user id holding markup as text', async () => {
    const evil = '<script>alert(1)</script>'
    const { dialog } = await openUnlink({
      [`GET ${IDS}`]: { json: { items: [{ ...linked[0], external_user_id: evil }] } },
    })
    expect(within(dialog).getByRole('heading').textContent).toBe(`Unlink chat user ${evil}`)
    expect(dialog.querySelector('script')).toBeNull()
  })

  it('names an empty chat user id and still sends the stored id', async () => {
    const { fetchFn, dialog } = await openUnlink({
      [`GET ${IDS}`]: { json: { items: [{ ...linked[0], external_user_id: '' }] } },
      [`DELETE ${IDS}/3`]: { json: { ok: true, id: 3 } },
    })
    expect(within(dialog).getByRole('heading').textContent).toBe('Unlink chat user (no id)')
    await answerConfirm()
    await waitFor(() => expect(deletes(fetchFn)).toHaveLength(1))
  })

  it.each([
    ['a 500', { status: 500, json: { error: 'store unavailable' } }, 'store unavailable'],
    ['a non-JSON body', { status: 502, text: '<html>bad gateway</html>' },
      'Failed to unlink chat user (502)'],
    ['a network error', () => Promise.reject(new TypeError('Failed to fetch')),
      'Failed to unlink chat user: Failed to fetch'],
  ])('keeps the dialog open with the error on %s', async (_name, fail, text) => {
    const { fetchFn } = await openUnlink({ [`DELETE ${IDS}/3`]: fail })
    await answerConfirm()
    const error = await screen.findByTestId('confirm-dialog-error')
    expect(error).toHaveTextContent(text)
    expect(error).not.toHaveTextContent('<html>')
    const dialog = screen.getByRole('dialog')
    expect(dialog).toContainElement(error)
    expect(dialog).toHaveAttribute('open')
    expect(deletes(fetchFn)).toHaveLength(1)
    expect(screen.getAllByTestId('chat-identity-row')).toHaveLength(2)
  })

  it('Escape sends nothing and returns focus to the Unlink button', async () => {
    const user = userEvent.setup()
    const fetchFn = stubFetch({ [`GET ${IDS}`]: { json: { items: linked } },
      [USERS]: { json: { users } } })
    render(<ChatIdentities />)
    const rows = await screen.findAllByTestId('chat-identity-row')
    const button = within(rows[0]).getByTestId('chat-identity-unlink')
    await user.click(button)
    expect(screen.getByRole('dialog')).toHaveAttribute('open')
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(button)
    expect(deletes(fetchFn)).toHaveLength(0)
  })

  it('sends once when Confirm is clicked again while in flight', async () => {
    let release
    const { fetchFn, dialog } = await openUnlink({
      [`DELETE ${IDS}/3`]: () => new Promise(res => { release = res }),
    })
    const confirmButton = within(dialog).getByTestId('confirm-dialog-confirm')
    fireEvent.click(confirmButton)
    fireEvent.click(confirmButton)
    await waitFor(() => expect(deletes(fetchFn)).toHaveLength(1))
    expect(confirmButton).toBeDisabled()
    release({ json: { ok: true, id: 3 } })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(deletes(fetchFn)).toHaveLength(1)
  })

  it('opens one dialog at a time and names the next row after Cancel', async () => {
    const { fetchFn } = await openUnlink({ [`DELETE ${IDS}/4`]: { json: { ok: true, id: 4 } } })
    const rows = screen.getAllByTestId('chat-identity-row')
    fireEvent.click(within(rows[1]).getByTestId('chat-identity-unlink'))
    expect(screen.getAllByRole('dialog')).toHaveLength(1)
    expect(await answerConfirm('cancel')).toBe('Unlink chat user U1')
    fireEvent.click(within(rows[1]).getByTestId('chat-identity-unlink'))
    expect(await answerConfirm()).toBe('Unlink chat user 4242')
    await waitFor(() => expect(deletes(fetchFn, 4)).toHaveLength(1))
    expect(deletes(fetchFn)).toHaveLength(0)
  })
})
