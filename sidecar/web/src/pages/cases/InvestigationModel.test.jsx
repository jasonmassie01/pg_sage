import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { InvestigationPanel } from './InvestigationPanel'

// Sage SRE M4: the Cases panel shows the model turn's output (AI-SRE-SPEC
// §9, CHECK-29/41). The model ranking is labeled and kept apart from the
// causal graph's scores (it is not a confidence); model-generated claims
// link to the exact evidence they cite; the model-proposed probe shows
// its rationale and evidence; and the timeline shows model_rejected and
// model_disagreed events with their reasons. Model text is untrusted and
// rendered as text.

const EV1 = '11111111-1111-4111-8111-111111111111'
const EV2 = '22222222-2222-4222-8222-222222222222'
const MISSING = '99999999-9999-4999-8999-999999999999'

const summary = {
  family: 'lock_blocking', conclusive: true, root: 'idle_in_tx_holder',
  subject: 'pid 4242', missing: [],
  model_ranking: { label: 'model ranking',
    basis: "model-generated order of the causal graph's open hypotheses; not a " +
      'confidence and not part of the deterministic diagnosis',
    nodes: ['idle_in_tx_holder', 'ddl_lock_queue'] },
  narrative: { label: 'model-generated narrative', claims: [
    { text: 'pid 4242 is idle in transaction and blocks 2 sessions.',
      evidence_ids: [EV1] },
    { text: '<img src=x onerror=alert(1)> the vacuum ran 30 s before.',
      evidence_ids: [EV2, MISSING] },
  ] },
  model_probe: { label: 'model-proposed probe', probe_id: 'long_transactions',
    rationale: 'tells an idle holder from an active one', evidence_id: EV2 },
}

const withModel = {
  database: 'orders',
  investigation: { id: 'inv-9', state: 'concluded', trigger_kind: 'lock_blocking',
    subject: 'incident 9', pinned: false, model_turns: 2, summary },
  hypotheses: [
    { node: 'idle_in_tx_holder', label: 'idle-in-transaction holder',
      status: 'root_cause', confidence: 0.85, subject: 'pid 4242',
      refutation_probe: 'lock_graph', operator_step: 'End the transaction.',
      support: [{ evidence_id: EV1, text: 'root blocker pid 4242 is idle' }],
      contradict: [] },
    { node: 'ddl_lock_queue', label: 'DDL queued behind a long transaction',
      status: 'unproven', confidence: 0.2, subject: 'pid 4242',
      refutation_probe: 'lock_graph', operator_step: 'Reschedule the DDL.',
      support: [], contradict: [] },
  ],
  evidence: [
    { id: EV1, probe_id: 'lock_graph', capability_state: 'available',
      observed_at: '2026-09-27T10:00:01Z', hash_verified: true, payload: { rows: [] } },
    { id: EV2, probe_id: 'long_transactions', capability_state: 'available',
      observed_at: '2026-09-27T10:00:09Z', hash_verified: true, payload: { rows: [] } },
  ],
  evidence_available: true, tombstones: [], chain_verified: true,
}

const events = {
  chain_verified: true,
  events: [
    { sequence: 1, type: 'created', actor: 'trigger', observed_at: '2026-09-27T10:00:00Z',
      payload: {} },
    { sequence: 5, type: 'model_rejected', actor: 'system',
      observed_at: '2026-09-27T10:00:05Z',
      payload: { reason: 'ungrounded_number', stage: 'review', turns: 1,
        detail: 'claim 2 uses 987654, which no cited evidence contains' } },
    { sequence: 6, type: 'model_disagreed', actor: 'system',
      observed_at: '2026-09-27T10:00:07Z',
      payload: { graph_root: 'idle_in_tx_holder', model_root: 'ddl_lock_queue',
        turns: 2 } },
    { sequence: 7, type: 'concluded', actor: 'system',
      observed_at: '2026-09-27T10:00:08Z', payload: { state: 'concluded' } },
  ],
}

let detail = withModel
let timeline = { data: events, error: null }
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => (url && url.endsWith('/events')
    ? { data: timeline.data, loading: false, error: timeline.error, refetch: vi.fn() }
    : { data: url ? detail : null, loading: false, error: null, refetch: vi.fn() }),
}))
vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

afterEach(() => {
  detail = withModel
  timeline = { data: events, error: null }
})

function open() {
  render(<InvestigationPanel database="orders" investigation={detail.investigation}
    user={{ role: 'viewer' }} />)
  fireEvent.click(screen.getByTestId('investigation-toggle'))
  return screen.getByTestId('investigation-detail')
}

describe('InvestigationPanel model output', () => {
  it('shows the model ranking labeled and apart from graph scores', () => {
    const panel = open()
    const ranking = within(panel).getByTestId('investigation-model-ranking')
    expect(ranking).toHaveTextContent('model ranking')
    expect(ranking).toHaveTextContent('not a confidence')
    const items = within(ranking).getAllByRole('listitem').map(li => li.textContent)
    expect(items[0]).toContain('idle-in-transaction holder')
    expect(items[1]).toContain('DDL queued behind a long transaction')
    expect(ranking).not.toHaveTextContent(/score|strong|moderate|weak/)
    expect(within(panel).getByTestId('investigation-likely'))
      .toHaveTextContent('score 0.85')
    expect(within(panel).getByTestId('investigation-model-turns'))
      .toHaveTextContent('Model turns: 2')
  })

  it('links each model-generated claim to the exact evidence it cites', () => {
    const panel = open()
    const narrative = within(panel).getByTestId('investigation-narrative')
    expect(narrative).toHaveTextContent('model-generated narrative')
    expect(narrative).toHaveTextContent('pid 4242 is idle in transaction')
    fireEvent.click(within(narrative).getByTestId(`evidence-link-${EV1}`))
    expect(within(panel).getByTestId(`evidence-${EV1}`).open).toBe(true)
    expect(within(narrative).getByTestId(`evidence-missing-${MISSING}`))
      .toHaveTextContent('evidence unavailable')
  })

  it('renders model text as text, never as markup', () => {
    const panel = open()
    const narrative = within(panel).getByTestId('investigation-narrative')
    expect(narrative).toHaveTextContent('<img src=x onerror=alert(1)>')
    expect(narrative.querySelector('img')).toBeNull()
  })

  it('shows the model-proposed probe with its rationale and evidence', () => {
    const panel = open()
    const probe = within(panel).getByTestId('investigation-model-probe')
    expect(probe).toHaveTextContent('model-proposed probe')
    expect(probe).toHaveTextContent('long_transactions')
    expect(probe).toHaveTextContent('tells an idle holder from an active one')
    expect(within(probe).getByTestId(`evidence-link-${EV2}`)).toBeInTheDocument()
  })

  it('shows model_rejected and model_disagreed in the timeline', () => {
    const panel = open()
    const tl = within(panel).getByTestId('investigation-timeline')
    const rejected = within(tl).getByTestId('timeline-model_rejected')
    expect(rejected).toHaveTextContent('ungrounded_number')
    expect(rejected).toHaveTextContent('review')
    expect(rejected).toHaveTextContent('987654')
    expect(rejected.dataset.kind).toBe('model')
    const disagreed = within(tl).getByTestId('timeline-model_disagreed')
    expect(disagreed).toHaveTextContent('idle_in_tx_holder')
    expect(disagreed).toHaveTextContent('ddl_lock_queue')
    expect(disagreed).toHaveTextContent(/graph.*stands/i)
    expect(within(tl).getByTestId('timeline-concluded').dataset.kind).toBe('graph')
    const order = within(tl).getAllByRole('listitem').map(li => li.dataset.testid)
    expect(order).toEqual(['timeline-created', 'timeline-model_rejected',
      'timeline-model_disagreed', 'timeline-concluded'])
  })

  it('shows no model sections for a deterministic investigation', () => {
    const plain = { ...summary }
    delete plain.model_ranking
    delete plain.narrative
    delete plain.model_probe
    detail = { ...withModel, investigation: { ...withModel.investigation,
      model_turns: 0, summary: plain } }
    timeline = { data: { chain_verified: true, events: [events.events[0]] }, error: null }
    const panel = open()
    expect(within(panel).queryByTestId('investigation-model-ranking')).toBeNull()
    expect(within(panel).queryByTestId('investigation-narrative')).toBeNull()
    expect(within(panel).queryByTestId('investigation-model-probe')).toBeNull()
    expect(within(panel).queryByTestId('investigation-model-turns')).toBeNull()
    expect(within(panel).getByTestId('timeline-created')).toBeInTheDocument()
  })

  it('says when the timeline cannot be read', () => {
    timeline = { data: null, error: '503 Service Unavailable' }
    const panel = open()
    expect(within(panel).getByTestId('investigation-timeline'))
      .toHaveTextContent('Timeline unavailable')
  })

  it('handles an empty ranking and a narrative without claims', () => {
    detail = { ...withModel, investigation: { ...withModel.investigation,
      summary: { ...summary, model_ranking: { ...summary.model_ranking, nodes: [] },
        narrative: { label: 'model-generated narrative', claims: [] } } } }
    const panel = open()
    expect(within(panel).queryByTestId('investigation-model-ranking')).toBeNull()
    expect(within(panel).queryByTestId('investigation-narrative')).toBeNull()
  })

  it('names a ranked node the graph does not list by its id', () => {
    detail = { ...withModel, investigation: { ...withModel.investigation,
      summary: { ...summary, model_ranking: { ...summary.model_ranking,
        nodes: ['idle_in_tx_holder', 'unknown_node_x'] } } } }
    const panel = open()
    expect(within(panel).getByTestId('investigation-model-ranking'))
      .toHaveTextContent('unknown_node_x')
  })
})
