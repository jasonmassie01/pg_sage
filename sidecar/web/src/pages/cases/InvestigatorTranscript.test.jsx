import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { InvestigatorTranscript } from './InvestigatorTranscript'

// Roadmap 2.1: the investigation view shows the tool-calling
// investigator's plan, each tool call with its result, digest and
// evidence link, which results the conclusion cites, the model's outcome
// with its authority (advisory unless earned), dropped claims and why
// the loop stopped. Model text is untrusted and rendered as text.

const EV1 = '11111111-1111-4111-8111-111111111111'
const EV5 = '55555555-5555-4555-8555-555555555555'
const DIGEST = 'ab'.repeat(32)

const run = {
  label: 'model investigator transcript', plan: 'narrow', protocol: 'native',
  budget: { max_steps: 5, max_probes: 3, wall_ms: 40000, max_tokens: 24000 },
  model_plan: 'Re-read the long transactions, then <b>conclude</b>.',
  steps: [
    { seq: 1, call: 1, tool: 'graph_state', status: 'ok', elapsed_ms: 2 },
    { seq: 2, call: 2, tool: 'run_probe', args: { probe: 'long_transactions', args: {} },
      status: 'ok', evidence_id: EV5, digest: DIGEST, cost: 1, elapsed_ms: 31 },
    { seq: 3, call: 2, tool: 'run_sql', status: 'rejected', note: 'forbidden_tool' },
    { seq: 4, call: 3, tool: 'submit_conclusion', status: 'final' },
  ],
  model_calls: 3, tool_calls: 3, probes: 1, tokens: 2400,
  rejected: { forbidden_tool: 1 }, dropped_claims: { unknown_evidence: 1 }, stop: 'final',
}

const evidence = [
  { id: EV1, probe_id: 'lock_graph', capability_state: 'available', hash_verified: true,
    payload: { status: 'ok', rows: [{}, {}] } },
  { id: EV5, probe_id: 'long_transactions', capability_state: 'available',
    hash_verified: true, payload: { status: 'ok', rows: [{ pid: 4242 }] } },
]

function summaryWith(conclusion, extra = {}) {
  return { investigator: run, model_conclusion: conclusion,
    narrative: { label: 'model-generated narrative', claims: [
      { text: 'pid 4242 has kept its transaction open for 95 s.', evidence_ids: [EV5] }] },
    ...extra }
}

function show(summary, onCite = vi.fn()) {
  render(<InvestigatorTranscript summary={summary} evidence={evidence} onCite={onCite} />)
  return screen.getByTestId('investigator')
}

describe('InvestigatorTranscript', () => {
  it('shows the plan with its budget and the model stated plan as text', () => {
    const box = show(summaryWith({ outcome: 'agreed', root: 'idle_in_tx_holder',
      authority: 'advisory', reason: 'agrees' }))
    const plan = within(box).getByTestId('investigator-plan')
    expect(plan).toHaveTextContent('narrow')
    expect(plan).toHaveTextContent('5 model steps')
    expect(plan).toHaveTextContent('3 probes')
    expect(plan).toHaveTextContent('40 s')
    const said = within(box).getByTestId('investigator-model-plan')
    expect(said).toHaveTextContent('<b>conclude</b>')
    expect(said.querySelector('b')).toBeNull()
  })

  it('lists each tool call with its result, digest and evidence link', () => {
    const onCite = vi.fn()
    const box = show(summaryWith({ outcome: 'agreed', authority: 'advisory',
      reason: 'x' }), onCite)
    const probe = within(box).getByTestId('investigator-step-2')
    expect(probe).toHaveTextContent('run_probe')
    expect(probe).toHaveTextContent('long_transactions')
    expect(probe).toHaveTextContent('ok, 1 row')
    expect(within(probe).getByTestId('investigator-digest-2'))
      .toHaveTextContent(DIGEST.slice(0, 12))
    fireEvent.click(within(probe).getByTestId(`evidence-link-${EV5}`))
    expect(onCite).toHaveBeenCalledWith(EV5)
    expect(within(probe).getByTestId('investigator-cited-2')).toHaveTextContent('cited')
    const rejected = within(box).getByTestId('investigator-step-3')
    expect(rejected).toHaveTextContent('run_sql')
    expect(rejected).toHaveTextContent('refused: forbidden_tool')
    expect(within(box).getByTestId('investigator-step-1')).not
      .toHaveTextContent('cited')
  })

  it('says a contest stays advisory and names both roots', () => {
    const box = show(summaryWith({ outcome: 'contested', root: 'ddl_lock_queue',
      graph_root: 'idle_in_tx_holder', authority: 'advisory',
      reason: 'needs 16 more correct held-out overrides' }))
    const out = within(box).getByTestId('investigator-outcome')
    expect(out).toHaveTextContent('ddl_lock_queue')
    expect(out).toHaveTextContent('idle_in_tx_holder')
    expect(out).toHaveTextContent(/advisory/i)
    expect(out).toHaveTextContent('needs 16 more correct held-out overrides')
    expect(out.dataset.authority).toBe('advisory')
  })

  it('says when an earned authority adopted the model root', () => {
    const box = show(summaryWith({ outcome: 'concluded', root: 'hot_row_contention',
      authority: 'adopted', reason: '16/16 held out' }))
    const out = within(box).getByTestId('investigator-outcome')
    expect(out).toHaveTextContent('hot_row_contention')
    expect(out).toHaveTextContent(/adopted/i)
    expect(out.dataset.authority).toBe('adopted')
  })

  it('shows an unmodeled cause as advisory with its mechanism', () => {
    const box = show(summaryWith({ outcome: 'unmodeled', authority: 'advisory',
      graph_root: 'idle_in_tx_holder', reason: 'no graph node',
      cause: { label: 'deploy job', mechanism: 'a <i>deploy</i> paused mid-transaction' } }))
    const out = within(box).getByTestId('investigator-outcome')
    expect(out).toHaveTextContent('deploy job')
    expect(out).toHaveTextContent('a <i>deploy</i> paused mid-transaction')
    expect(out.querySelector('i')).toBeNull()
    expect(out).toHaveTextContent(/advisory/i)
  })

  it('shows why the loop stopped, refused calls and dropped claims', () => {
    const stopped = { ...run, stop: 'max_steps', dropped_claims: { uncited: 2 } }
    const box = show({ investigator: stopped })
    const stop = within(box).getByTestId('investigator-stop')
    expect(stop).toHaveTextContent('max_steps')
    expect(stop).toHaveTextContent(/graph's diagnosis stands/i)
    expect(within(box).getByTestId('investigator-dropped'))
      .toHaveTextContent('2 claims dropped (uncited 2)')
    expect(within(box).getByTestId('investigator-rejected'))
      .toHaveTextContent('forbidden_tool 1')
    expect(within(box).queryByTestId('investigator-outcome')).toBeNull()
  })

  it('marks evidence that is no longer stored', () => {
    render(<InvestigatorTranscript summary={summaryWith(null)} evidence={[]}
      onCite={vi.fn()} />)
    const step = screen.getByTestId('investigator-step-2')
    expect(within(step).getByTestId(`evidence-missing-${EV5}`))
      .toHaveTextContent('evidence unavailable')
  })

  it('renders nothing without an investigator run', () => {
    const { container } = render(<InvestigatorTranscript summary={{}} evidence={[]}
      onCite={vi.fn()} />)
    expect(container).toBeEmptyDOMElement()
  })
})
