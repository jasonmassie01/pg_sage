import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AgentDBsPage } from './AgentDBsPage'

// D4: the dashboard provisions by consuming the approved request, never by
// registering the deployment directly.

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(() => ({
    data: null, loading: false, error: null, refetch: () => Promise.resolve(),
  })),
}))

function jsonResponse(body) {
  return { ok: true, json: async () => body }
}

function submitProvisionForm() {
  fireEvent.click(screen.getByRole('tab', { name: 'Provision' }))
  fireEvent.click(screen.getByTestId('agent-db-submit'))
}

describe('AgentDBsPage request-based provisioning', () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn()
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('provisions through the approved request', async () => {
    globalThis.fetch
      .mockResolvedValueOnce(jsonResponse({
        request_id: 'req_approved_ui', status: 'approved', policy_decision: 'allow',
      }))
      .mockResolvedValueOnce(jsonResponse({ deployment_id: 'dep_from_request' }))

    render(<AgentDBsPage />)
    submitProvisionForm()

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    const urls = globalThis.fetch.mock.calls.map(call => call[0])
    expect(urls[1]).toBe('/api/v1/agent-dbs/requests/req_approved_ui/provision')
    expect(urls).not.toContain('/api/v1/agent-dbs')
    const body = JSON.parse(globalThis.fetch.mock.calls[1][1].body)
    expect(body).toEqual(expect.objectContaining({
      deployment_id: expect.any(String),
      size_profile_id: expect.any(String),
      schema_name: expect.any(String),
      lease_seconds: expect.any(Number),
    }))
    expect(await screen.findByText('Provisioned dep_from_request'))
      .toBeInTheDocument()
  })

  it('stops without provisioning when the request is not approved', async () => {
    globalThis.fetch.mockResolvedValueOnce(jsonResponse({
      request_id: 'req_review_ui', status: 'requested', policy_decision: 'review',
    }))

    render(<AgentDBsPage />)
    submitProvisionForm()

    expect(await screen.findByText('Request requested: review')).toBeInTheDocument()
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
  })

  it('shows the server error when the approval was already used', async () => {
    globalThis.fetch
      .mockResolvedValueOnce(jsonResponse({
        request_id: 'req_used_ui', status: 'approved', policy_decision: 'allow',
      }))
      .mockResolvedValueOnce({
        ok: false,
        status: 409,
        json: async () => ({ error: 'agent db request already consumed' }),
        text: async () => '{"error":"agent db request already consumed"}',
      })

    render(<AgentDBsPage />)
    submitProvisionForm()

    expect(await screen.findByText(/already consumed/)).toBeInTheDocument()
    expect(screen.queryByText(/^Provisioned /)).not.toBeInTheDocument()
  })
})
