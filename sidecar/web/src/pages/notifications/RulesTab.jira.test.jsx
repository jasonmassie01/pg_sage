import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { RulesTab } from './RulesTab'
import { stubFetch } from '../../test/fetchStub'

// market-14: a Jira integration is a case link, so the rule form never
// offers it as a channel (the API refuses such a rule too).
// No concurrent access tests: one render, one load.

afterEach(() => vi.unstubAllGlobals())

describe('RulesTab and ticket integrations', () => {
  it('offers alert channels but not the Jira integration', async () => {
    stubFetch({
      '/api/v1/notifications/channels': { json: { channels: [
        { id: 1, name: 'tickets', type: 'jira' },
        { id: 2, name: 'dba-teams', type: 'teams' },
      ] } },
      '/api/v1/notifications/rules': { json: { rules: [] } },
    })
    render(<RulesTab />)
    const select = await screen.findByTestId('add-rule-channel')
    await waitFor(() => expect(select.options.length).toBe(1))
    expect(select.options[0].textContent).toBe('dba-teams')
  })
})
