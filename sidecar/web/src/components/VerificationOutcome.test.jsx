import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { VerificationOutcome } from './VerificationOutcome'

// Phase 1.3: an executed action shows what pg_sage predicted, what it
// observed, the verdict and the evidence behind it.

const improved = {
  class: 'index_create', verdict: 'improved', tolerance: 'met',
  predicted: {
    method: 'hypopg', metric: 'mean_exec_time', expected_change_pct: -40,
    target_queryids: [77, 78],
  },
  observed: { metric: 'mean_exec_time', before: 20, after: 9, change_pct: -55 },
  evidence: {
    comparison: {
      before: { calls: 1200, mean_ms: 20 }, after: { calls: 1100, mean_ms: 9 },
      ci_low_pct: -60.2, ci_high_pct: -49.8,
    },
  },
  reason: 'call-weighted mean fell 55%',
  window_start: '2026-10-03T00:00:00Z', window_end: '2026-10-03T02:00:00Z',
}

describe('VerificationOutcome', () => {
  it('renders nothing without an outcome', () => {
    const { container } = render(<VerificationOutcome outcome={null} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows predicted vs observed with the verdict', () => {
    render(<VerificationOutcome outcome={improved} />)
    const panel = screen.getByTestId('verification-outcome')
    expect(within(panel).getByTestId('outcome-verdict')).toHaveTextContent('Improved')
    expect(within(panel).getByTestId('outcome-predicted')).toHaveTextContent('-40%')
    expect(within(panel).getByTestId('outcome-predicted')).toHaveTextContent('HypoPG')
    expect(within(panel).getByTestId('outcome-observed')).toHaveTextContent('-55%')
    expect(within(panel).getByTestId('outcome-tolerance')).toHaveTextContent('Met')
    expect(within(panel).getByText('call-weighted mean fell 55%')).toBeInTheDocument()
    expect(within(panel).getByTestId('outcome-evidence')).toHaveTextContent('1200')
    expect(within(panel).getByTestId('outcome-evidence')).toHaveTextContent('-60.2')
    expect(within(panel).getByTestId('outcome-targets')).toHaveTextContent('77, 78')
  })

  it('says when there was no prediction', () => {
    render(<VerificationOutcome outcome={{
      class: 'guc', verdict: 'unverifiable', tolerance: 'no_prediction',
      predicted: { method: 'none', note: 'advisor gave no expected effect' },
      observed: {}, evidence: {}, reason: 'no prediction: not credited',
    }} />)
    expect(screen.getByTestId('outcome-verdict')).toHaveTextContent('Unverifiable')
    expect(screen.getByTestId('outcome-predicted')).toHaveTextContent('No prediction')
    expect(screen.getByTestId('outcome-observed')).toHaveTextContent('Not measured')
  })

  it('explains a soft-drop re-create', () => {
    render(<VerificationOutcome outcome={{
      ...improved, class: 'index_drop', verdict: 'regressed', tolerance: 'missed',
      predicted: { method: 'rule', metric: 'mean_exec_time', expected_change_pct: 0 },
      observed: { metric: 'mean_exec_time', before: 10, after: 30, change_pct: 200 },
      evidence: { soft_drop: { trigger: 'query_regression', recreated: true,
        definition: 'CREATE INDEX i ON t (a)' } },
    }} />)
    expect(screen.getByTestId('outcome-verdict')).toHaveTextContent('Regressed')
    expect(screen.getByTestId('outcome-soft-drop')).toHaveTextContent('re-created')
    expect(screen.getByTestId('outcome-soft-drop')).toHaveTextContent('query regression')
  })

  it('shows a pending verification with its prediction', () => {
    render(<VerificationOutcome outcome={{
      class: 'index_drop', verdict: 'pending', tolerance: 'pending',
      predicted: { method: 'rule', metric: 'mean_exec_time', expected_change_pct: 0 },
      observed: {}, evidence: {},
    }} />)
    expect(screen.getByTestId('outcome-verdict')).toHaveTextContent('Verifying')
    expect(screen.getByTestId('outcome-predicted')).toHaveTextContent('0%')
  })

  it('labels insufficient evidence as not credited', () => {
    render(<VerificationOutcome outcome={{
      ...improved, verdict: 'insufficient_evidence', tolerance: 'unmeasured',
    }} />)
    expect(screen.getByTestId('outcome-verdict')).toHaveTextContent('Inconclusive')
    expect(screen.getByTestId('verification-outcome')).toHaveTextContent('not credited')
  })
})

// lab-01c: verification v2's verdicts, method and shadow line.
const v2Evidence = {
  verifier: 'v2', method: 'did', basis: 'pre_window', effect_pct: -38,
  ci_low_pct: -45.1, ci_high_pct: -30.2, controls: 12, outcome: 'improved',
}

describe('VerificationOutcome v2', () => {
  it('labels neutral "No change" and insufficient evidence "Inconclusive"', () => {
    const { rerender } = render(<VerificationOutcome outcome={{
      ...improved, verdict: 'neutral' }} />)
    expect(screen.getByTestId('outcome-verdict')).toHaveTextContent('No change')
    rerender(<VerificationOutcome outcome={{ ...improved,
      verdict: 'insufficient_evidence' }} />)
    expect(screen.getByTestId('outcome-verdict')).toHaveTextContent('Inconclusive')
  })

  it('shows the method and the effect with its CI against the controls', () => {
    render(<VerificationOutcome outcome={{ ...improved, verifier: 'v2',
      evidence: { ...improved.evidence, v2: v2Evidence } }} />)
    expect(screen.getByTestId('outcome-method')).toHaveTextContent('Verifier v2')
    expect(screen.getByTestId('outcome-effect')).toHaveTextContent(
      '-38.0% (CI -45.1% to -30.2%) vs 12 control statements')
    expect(screen.queryByTestId('outcome-shadow')).toBeNull()
  })

  it('in shadow, says what v2 would say under the acted-on verdict', () => {
    render(<VerificationOutcome outcome={{ ...improved, verifier: 'v1',
      other_verdict: 'neutral', other_evidence: { v2: { ...v2Evidence,
        outcome: 'neutral', effect_pct: -2, ci_low_pct: -6, ci_high_pct: 3 } },
      evidence: { ...improved.evidence, v2: { ...v2Evidence, outcome: 'neutral',
        effect_pct: -2, ci_low_pct: -6, ci_high_pct: 3 } } }} />)
    expect(screen.getByTestId('outcome-verdict')).toHaveTextContent('Improved')
    expect(screen.getByTestId('outcome-method')).toHaveTextContent('Verifier v1')
    expect(screen.getByTestId('outcome-shadow')).toHaveTextContent(
      'v2 would say: No change')
    expect(screen.getByTestId('outcome-effect')).toHaveTextContent(
      '-2.0% (CI -6.0% to 3.0%) vs 12 control statements')
  })

  it('shows no effect line without a measured v2 record', () => {
    render(<VerificationOutcome outcome={{ ...improved, verifier: 'v1' }} />)
    expect(screen.queryByTestId('outcome-effect')).toBeNull()
    expect(screen.queryByTestId('outcome-shadow')).toBeNull()
    render(<VerificationOutcome outcome={{ ...improved, other_verdict: 'regressed',
      other_evidence: {} }} />)
    expect(screen.getByTestId('outcome-shadow')).toHaveTextContent(
      'v2 would say: Regressed')
  })

  it('says when v2 had no control group', () => {
    render(<VerificationOutcome outcome={{ ...improved, verifier: 'v2', evidence: {
      v2: { ...v2Evidence, method: 'its', controls: 0 } } }} />)
    expect(screen.getByTestId('outcome-effect')).toHaveTextContent(
      '-38.0% (CI -45.1% to -30.2%), no control group')
  })
})
