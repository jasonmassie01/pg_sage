import { describe, expect, it } from 'vitest'
import {
  dependants, expectedText, gateText, haltSummary, measuredText, outcomesSince, planHref,
  planParam, stageStatus, tierText, upToChoices,
} from './plans'
import { coverageFor, planStep, proposedPlan, withStates } from './planTestData'

// ux-03: the plan review's pure rules. No concurrent access tests here:
// every helper is a pure function of its arguments.

describe('planHref and planParam', () => {
  it('links to the plan on the Plans tab with its database', () => {
    expect(planHref(7, 'orders db')).toBe('#/actions?tab=plans&plan=7&database=orders+db')
    expect(planHref(7, '')).toBe('#/actions?tab=plans&plan=7')
  })

  it('reads only a positive integer plan id', () => {
    expect(planParam('#/actions?tab=plans&plan=7')).toBe('7')
    for (const bad of ['#/actions?plan=0', '#/actions?plan=-1', '#/actions?plan=7x',
      '#/actions?plan=07', '#/actions', '', '#/actions?plan=99999999999999999999']) {
      expect(planParam(bad), bad).toBe('')
    }
  })
})

describe('upToChoices', () => {
  const steps = [1, 2, 3, 4, 5].map(n => planStep(n * 10, n, `i${n}`))

  it('offers every stage but the last when none is never-automatic', () => {
    expect(upToChoices(steps, coverageFor(steps))).toEqual([1, 2, 3, 4])
  })

  it('stops before the first never-automatic stage', () => {
    const cov = coverageFor(steps, { 3: { needs_own_approval: true } })
    expect(upToChoices(steps, cov)).toEqual([1, 2])
  })

  it('offers nothing when stage 1 is never-automatic, or for one or zero stages', () => {
    expect(upToChoices(steps, coverageFor(steps, { 1: { needs_own_approval: true } })))
      .toEqual([])
    expect(upToChoices(steps.slice(0, 1), coverageFor(steps.slice(0, 1)))).toEqual([])
    expect(upToChoices([], [])).toEqual([])
    expect(upToChoices(null, null)).toEqual([])
  })
})

describe('dependants', () => {
  it('follows edges transitively, in ordinal order, without the stage itself', () => {
    const edges = [{ from: 1, to: 3 }, { from: 3, to: 2 }, { from: 2, to: 4 },
      { from: 5, to: 6 }]
    expect(dependants(edges, 1)).toEqual([2, 3, 4])
    expect(dependants(edges, 4)).toEqual([])
    expect(dependants(null, 1)).toEqual([])
  })

  it('ends on a cycle', () => {
    expect(dependants([{ from: 1, to: 2 }, { from: 2, to: 1 }], 1)).toEqual([2])
  })
})

describe('haltSummary', () => {
  it('names the halted stage, what was reverted, kept and never started', () => {
    const steps = [1, 2, 3, 4, 5].map(n => planStep(n * 10, n, `i${n}`))
    const detail = withStates(proposedPlan({ steps }), 'halted', {
      1: { state: 'kept', verdict: 'improved' }, 2: { state: 'kept', verdict: 'no_change' },
      3: { state: 'reverted', verdict: 'regressed', state_reason: 'blocks per call rose 40%' },
    })
    expect(haltSummary(detail.plan)).toEqual({ halted: 3, reason: 'blocks per call rose 40%',
      reverted: [3], kept: [1, 2], notStarted: [4, 5] })
  })

  it('prefers a stage in the halted state', () => {
    const detail = withStates(proposedPlan(), 'halted', {
      1: { state: 'kept' }, 2: { state: 'halted', state_reason: 'inconclusive' } })
    expect(haltSummary(detail.plan)).toMatchObject({ halted: 2, reason: 'inconclusive',
      kept: [1], reverted: [], notStarted: [3] })
  })

  it('names the regressed stage, not the checkpoint walk, after revert_to_checkpoint', () => {
    // Stage 4 regressed; the walk back to the checkpoint at 1 also reverted
    // 2 and 3, which had been kept. The plan halted at stage 4.
    const steps = [1, 2, 3, 4, 5].map(n => planStep(n * 10, n, `i${n}`))
    const walk = 'revert_to_checkpoint from step 4'
    const detail = withStates(proposedPlan({ steps }), 'halted', {
      1: { state: 'kept', verdict: 'improved' },
      2: { state: 'reverted', verdict: 'improved', state_reason: walk },
      3: { state: 'reverted', verdict: 'no_change', state_reason: walk },
      4: { state: 'reverted', verdict: 'regressed', state_reason: 'time per call rose 25%' },
    })
    expect(haltSummary(detail.plan)).toEqual({ halted: 4, reason: 'time per call rose 25%',
      reverted: [2, 3, 4], kept: [1], notStarted: [5] })
  })

  it('names a stage whose revert failed over the stages reverted before it', () => {
    const detail = withStates(proposedPlan(), 'halted', {
      1: { state: 'failed', state_reason: 'the revert did not land' },
      2: { state: 'reverted', verdict: 'regressed' } })
    expect(haltSummary(detail.plan)).toMatchObject({ halted: 1,
      reason: 'the revert did not land', reverted: [2] })
  })

  it('is null for a plan that is not halted', () => {
    expect(haltSummary(proposedPlan().plan)).toBeNull()
    expect(haltSummary(null)).toBeNull()
  })
})

describe('stage words', () => {
  it('says each runtime state in words, and verified for a kept improvement', () => {
    expect(stageStatus({ state: 'pending' })).toBe('Not started')
    expect(stageStatus({ state: 'running' })).toBe('Running')
    expect(stageStatus({ state: 'kept', verdict: 'improved' })).toBe('Verified: kept')
    expect(stageStatus({ state: 'kept', verdict: 'no_change' })).toBe('Kept: no change')
    expect(stageStatus({ state: 'reverted' })).toBe('Reverted')
    expect(stageStatus({ state: 'awaiting_operator' })).toBe('Waiting for you to run it')
    expect(stageStatus({ state: 'mystery' })).toBe('mystery')
    expect(stageStatus(undefined)).toBe('Not started')
  })

  it('says the expected effect with or without a size', () => {
    expect(expectedText(planStep(1, 1, 'a', { expected_pct: 30 })))
      .toBe('blocks per call down 30%')
    expect(expectedText(planStep(1, 1, 'a'))).toBe('blocks per call down (size not estimated)')
    expect(expectedText(planStep(1, 1, 'a', { expected_direction: 'none',
      metric: 'readback' }))).toBe('setting reads back as set')
  })

  it('says the measured effect, or not measured yet', () => {
    expect(measuredText({ measured: { metric: 'blocks_per_call', change_pct: -32.46 } }))
      .toBe('blocks per call -32.5%')
    expect(measuredText({ measured: null, measured_reason: 'too few calls' }))
      .toBe('not measured yet: too few calls')
    expect(measuredText(undefined)).toBe('not measured yet')
  })

  it('names the ladder tier from the ledger row, else from the expected source', () => {
    expect(tierText(planStep(1, 1, 'a'), { tier: 'clone' })).toBe('Tested on a clone')
    expect(tierText(planStep(1, 1, 'a'), undefined)).toBe('Tested on plans only')
    expect(tierText(planStep(1, 1, 'a', { expected_source: 'canary' })))
      .toBe('Tested in production')
    expect(tierText(planStep(1, 1, 'a', { expected_source: 'none' })))
      .toBe('Not tested before production')
  })

  it('says the gate to the next stage: automatic, a person, or the end', () => {
    const s = planStep(1, 1, 'a')
    expect(gateText(s, { covered: true, needs_own_approval: false }))
      .toMatch(/^Automatic: the next stage starts once this one is judged improved or no change/)
    expect(gateText(s, { covered: false, needs_own_approval: true,
      reason: 'drops an object' })).toBe('A person approves the next stage: drops an object')
    expect(gateText(s, null)).toBe('Last stage: the plan completes once it is judged')
  })
})

describe('outcomesSince', () => {
  it('covers the hours since the plan was made, within the ledger window', () => {
    const now = Date.parse('2026-11-16T12:00:00Z')
    expect(outcomesSince('2026-11-16T09:30:00Z', now)).toBe('4h')
    expect(outcomesSince('2020-01-01T00:00:00Z', now)).toBe('9600h')
    expect(outcomesSince('not a time', now)).toBe('9600h')
    expect(outcomesSince('2026-11-17T00:00:00Z', now)).toBe('1h')
  })
})
