import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { WhyNotPanel } from './WhyNotPanel'

// ux-08b review: the dashboard is often served over plain http on a LAN,
// where browsers expose no navigator.clipboard. Copy must then say so and
// leave the statement selectable, not throw inside the click handler.
// No concurrency test: one click, no shared state.

const apiGet = vi.fn()
vi.mock('../lib/api', () => ({ apiGet: (...args) => apiGet(...args) }))

const GRANT = 'GRANT pg_read_all_stats TO sage;'
const answer = {
  database: 'alpha', first_fail: 'privilege', failing: ['privilege'],
  links: [{ name: 'privilege', status: 'fail', first_fail: true,
    sentence: 'The sage role cannot read other roles’ query text.',
    fix: { kind: 'grant_sql', value: GRANT } }],
}

let original
beforeEach(() => {
  apiGet.mockReset()
  original = Object.getOwnPropertyDescriptor(navigator, 'clipboard')
})
afterEach(() => {
  if (original) Object.defineProperty(navigator, 'clipboard', original)
  else delete navigator.clipboard
})

async function clickCopy() {
  apiGet.mockResolvedValue(answer)
  render(<WhyNotPanel database="alpha" findingId="17" />)
  fireEvent.click(await screen.findByRole('button', { name: /copy/i }))
}

describe('WhyNotPanel copy without a clipboard', () => {
  it('says copy is unavailable and keeps the statement on screen', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })
    await clickCopy()
    expect(await screen.findByRole('button', { name: /copy is unavailable here/i }))
      .toBeInTheDocument()
    expect(screen.getByText(GRANT).tagName).toBe('CODE')
  })

  it('names the error when the browser refuses the write', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('Document is not focused'))
    Object.defineProperty(navigator, 'clipboard', { value: { writeText },
      configurable: true })
    await clickCopy()
    expect(writeText).toHaveBeenCalledWith(GRANT)
    expect(await screen.findByRole('button',
      { name: 'Copy failed (Document is not focused); select it' })).toBeInTheDocument()
  })
})
