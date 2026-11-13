import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ChannelForm } from './ChannelForm'
import { stubFetch } from '../../test/fetchStub'

// market-14: Teams, Opsgenie and incident.io are channel types. Their
// credentials are password inputs; routing values are plain text.
// No concurrent access tests: one submit per click, local form state only.

afterEach(() => vi.unstubAllGlobals())

const CHANNELS = '/api/v1/notifications/channels'

async function create(type, fields) {
  const fetchFn = stubFetch({
    [`POST ${CHANNELS}`]: { status: 201, json: { id: 3, name: 'n', type } },
  })
  render(<ChannelForm onSaved={vi.fn()} />)
  fireEvent.change(screen.getByTestId('add-channel-name'), { target: { value: 'n' } })
  fireEvent.change(screen.getByTestId('add-channel-type'), { target: { value: type } })
  for (const [id, value] of Object.entries(fields)) {
    fireEvent.change(screen.getByTestId(`add-channel-${id}`), { target: { value } })
  }
  fireEvent.click(screen.getByTestId('add-channel-submit'))
  await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1))
  return JSON.parse(fetchFn.mock.calls.at(-1)[1].body)
}

describe('ChannelForm new destinations', () => {
  it('offers Teams, Opsgenie and incident.io', () => {
    render(<ChannelForm onSaved={vi.fn()} />)
    const values = [...screen.getByTestId('add-channel-type').options].map(o => o.value)
    expect(values).toEqual(expect.arrayContaining(['teams', 'opsgenie', 'incidentio']))
  })

  it('creates a Teams channel with a secret webhook URL and a dashboard link', async () => {
    render(<ChannelForm onSaved={vi.fn()} />)
    fireEvent.change(screen.getByTestId('add-channel-type'), { target: { value: 'teams' } })
    expect(screen.getByTestId('add-channel-webhook-url')).toHaveAttribute('type', 'password')
    expect(screen.getByTestId('add-channel-dashboard-url')).toHaveAttribute('type', 'text')
  })

  it('posts a Teams channel', async () => {
    const body = await create('teams', {
      'webhook-url': 'https://prod.logic.azure.com/wf',
      'dashboard-url': 'https://sage.example.com',
    })
    expect(body).toEqual({ name: 'n', type: 'teams', config: {
      webhook_url: 'https://prod.logic.azure.com/wf', dashboard_url: 'https://sage.example.com',
    } })
  })

  it('posts an Opsgenie channel with its region, defaulting to us', async () => {
    const body = await create('opsgenie', { 'api-key': 'og', region: 'eu' })
    expect(body.config).toEqual({ api_key: 'og', region: 'eu' })
  })

  it('keeps the us region when the operator leaves it', async () => {
    const body = await create('opsgenie', { 'api-key': 'og' })
    expect(body.config).toEqual({ api_key: 'og', region: 'us' })
  })

  it('posts an incident.io channel', async () => {
    render(<ChannelForm onSaved={vi.fn()} />)
    fireEvent.change(screen.getByTestId('add-channel-type'), { target: { value: 'incidentio' } })
    expect(screen.getByTestId('add-channel-token')).toHaveAttribute('type', 'password')
  })

  it('sends the incident.io source id and token', async () => {
    const body = await create('incidentio', { 'alert-source-config-id': '01HX', token: 't' })
    expect(body.config).toEqual({ alert_source_config_id: '01HX', token: 't' })
  })
})

describe('ChannelForm Jira integration', () => {
  it('posts a Jira integration with a secret token', async () => {
    render(<ChannelForm onSaved={vi.fn()} />)
    fireEvent.change(screen.getByTestId('add-channel-type'), { target: { value: 'jira' } })
    expect(screen.getByTestId('add-channel-api-token')).toHaveAttribute('type', 'password')
  })

  it('sends the Jira settings', async () => {
    const body = await create('jira', {
      'base-url': 'https://acme.atlassian.net', email: 'bot@example.com',
      'api-token': 'tok', 'project-key': 'OPS',
    })
    expect(body.config).toEqual({ base_url: 'https://acme.atlassian.net',
      email: 'bot@example.com', api_token: 'tok', project_key: 'OPS' })
  })
})
