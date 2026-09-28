import { describe, expect, it, vi } from 'vitest'
import {
  fetchDeploymentPage,
  mergeDeployments,
  runLiveProvisionAction,
} from './agentDBLiveActions'

describe('agentDBLiveActions', () => {
  it('appends paginated deployments without duplicates (G8-B25)', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ deployments: [{ deployment_id: 'b' }], next_cursor: 'c2' }),
    })
    const page = await fetchDeploymentPage('c1')
    expect(globalThis.fetch.mock.calls[0][0]).toBe('/api/v1/agent-dbs?cursor=c1')
    const merged = mergeDeployments(
      [{ deployment_id: 'a' }, { deployment_id: 'b' }], page.deployments,
    )
    expect(merged.map(d => d.deployment_id)).toEqual(['a', 'b'])
    expect(page.next_cursor).toBe('c2')
  })

  it('never sends client-invented cost or approval claims (G8-B13)', async () => {
    const postJSON = vi.fn()
      .mockResolvedValueOnce({ plan_hash: 'h', estimate_id: 'e',
        authorization_id: 'a', idempotency_key: 'k', plan: {}, estimate: {} })
      .mockResolvedValueOnce({ status: 'succeeded' })
    await runLiveProvisionAction(postJSON, 'dep', 'live-execute', () => true)
    const body = postJSON.mock.calls[1][1]
    expect(body).toEqual({ plan_hash: 'h', estimate_id: 'e',
      authorization_id: 'a', idempotency_key: 'k' })
    expect(postJSON.mock.calls[1][2]).toEqual({ 'Idempotency-Key': 'k' })
  })
})
