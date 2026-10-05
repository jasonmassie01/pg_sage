import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { InvestigationPanel } from './InvestigationPanel'

// The investigation detail shows the tool-calling investigator's
// transcript next to the model output, and its cited claims open the
// evidence they cite.

const EV5 = '55555555-5555-4555-8555-555555555555'

const detail = {
  investigation: { id: 'inv-2', state: 'concluded', trigger_kind: 'lock_blocking',
    pinned: false, model_turns: 0, version: 3,
    summary: { family: 'lock_blocking', conclusive: true, root: 'idle_in_tx_holder',
      missing: [],
      narrative: { label: 'model-generated narrative', claims: [
        { text: 'pid 4242 has kept its transaction open for 95 s.', evidence_ids: [EV5] }] },
      model_conclusion: { label: 'model conclusion', outcome: 'agreed',
        root: 'idle_in_tx_holder', authority: 'advisory', reason: 'agrees' },
      investigator: { label: 'model investigator transcript', plan: 'broad',
        protocol: 'native', stop: 'final', model_calls: 2, tool_calls: 1, probes: 1,
        budget: { max_steps: 10, max_probes: 6, wall_ms: 90000, max_tokens: 48000 },
        steps: [{ seq: 1, call: 1, tool: 'run_probe', status: 'ok', evidence_id: EV5,
          digest: 'cd'.repeat(32), args: { probe: 'long_transactions' } }] } } },
  hypotheses: [],
  evidence: [{ id: EV5, probe_id: 'long_transactions', capability_state: 'available',
    hash_verified: true, payload: { status: 'ok', rows: [{ pid: 4242 }] } }],
  evidence_available: true, tombstones: [], chain_verified: true,
}

vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => (url && url.endsWith('/events')
    ? { data: { chain_verified: true, events: [] }, loading: false, error: null,
      refetch: vi.fn() }
    : { data: url ? detail : null, loading: false, error: null, refetch: vi.fn() }),
}))
vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

describe('InvestigationPanel with the investigator', () => {
  it('shows the transcript and opens cited evidence from a step', () => {
    render(<InvestigationPanel database="orders" investigation={detail.investigation}
      user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('investigation-toggle'))
    const panel = screen.getByTestId('investigation-detail')
    const box = within(panel).getByTestId('investigator')
    expect(within(box).getByTestId('investigator-plan')).toHaveTextContent('broad')
    fireEvent.click(within(within(box).getByTestId('investigator-step-1'))
      .getByTestId(`evidence-link-${EV5}`))
    expect(within(panel).getByTestId(`evidence-${EV5}`).open).toBe(true)
  })
})
