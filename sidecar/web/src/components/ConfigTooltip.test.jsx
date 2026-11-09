// ConfigTooltip.test.jsx — unit coverage for ConfigTooltip.
//
// Plan reference: reviews/archive/docs-internal/plan_v0.8.5.md §7.5 CHECK-T18 (component
// coverage) and CHECK-T19 (graceful degradation on unknown key).
//
// Test categories per global CLAUDE.md §5:
//   Happy path ............. known key renders tooltip content
//   Invalid input .......... unknown key → children rendered raw
//   Nil / empty ............ empty doc → children rendered raw
//   Boundary ............... warning field toggles callout + styling
//   State transitions ...... secret / mode / docs_url branches
//
// The generated config_meta.json is mocked so these tests do not
// break when Tier 1/2 doc strings are rewritten.

import React from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi, beforeEach } from 'vitest'

// vi.mock hoists above imports, so we stub the generated meta before
// ConfigTooltip is loaded.
vi.mock('../generated/config_meta.json', () => ({
  default: {
    'trust.level': {
      type: 'string',
      default: 'observation',
      doc: 'Autonomy tier for the sidecar.',
      warning: 'Autonomous executes DDL without review.',
    },
    'analyzer.slow_query_threshold_ms': {
      type: 'int',
      default: 1000,
      doc: 'Queries slower than this are flagged.',
      mode: 'standalone',
      docs_url: '/config#slow-query',
    },
    'llm.api_key': {
      type: 'string',
      default: null,
      doc: 'API key for LLM provider.',
      secret: true,
    },
    'empty.doc.field': {
      type: 'int',
      default: 0,
      doc: '',
    },
  },
}))

import { ConfigTooltip } from './ConfigTooltip'

describe('ConfigTooltip', () => {
  beforeEach(() => {
    // Fresh DOM — Radix Portal attaches to document.body.
    document.body.innerHTML = ''
  })

  it('renders children wrapped with data-config-key when meta exists', () => {
    render(
      <ConfigTooltip configKey="trust.level">
        <span>trust level</span>
      </ConfigTooltip>
    )
    const trigger = screen.getByText('trust level').parentElement
    expect(trigger).toHaveAttribute('data-config-key', 'trust.level')
    expect(trigger.className).toMatch(/border-dotted/)
  })

  it('graceful degradation: unknown key returns children unwrapped', () => {
    render(
      <ConfigTooltip configKey="does.not.exist">
        <span data-testid="raw">raw child</span>
      </ConfigTooltip>
    )
    const child = screen.getByTestId('raw')
    // Parent should NOT be the Radix trigger wrapper.
    expect(child.parentElement).not.toHaveAttribute('data-config-key')
  })

  it('empty doc string also triggers graceful degradation', () => {
    render(
      <ConfigTooltip configKey="empty.doc.field">
        <span data-testid="raw">zero-doc</span>
      </ConfigTooltip>
    )
    expect(
      screen.getByTestId('raw').parentElement,
    ).not.toHaveAttribute('data-config-key')
  })

  it('shows tooltip content on hover, including warning callout', async () => {
    const user = userEvent.setup()
    render(
      <ConfigTooltip configKey="trust.level">
        <span>trust level</span>
      </ConfigTooltip>
    )

    await user.hover(screen.getByText('trust level'))
    // Radix portal renders into document.body; use findAllByText to
    // wait for the async content open.
    const docs = await screen.findAllByText(
      'Autonomy tier for the sidecar.'
    )
    expect(docs.length).toBeGreaterThan(0)
    const warning = await screen.findAllByText(
      /Autonomous executes DDL/,
    )
    expect(warning.length).toBeGreaterThan(0)
  })

  it('shows mode badge and docs_url link when present', async () => {
    const user = userEvent.setup()
    render(
      <ConfigTooltip configKey="analyzer.slow_query_threshold_ms">
        <span>slow q</span>
      </ConfigTooltip>
    )
    await user.hover(screen.getByText('slow q'))
    const modeBadges = await screen.findAllByText('standalone')
    expect(modeBadges.length).toBeGreaterThan(0)
    const links = await screen.findAllByRole('link', {
      name: /Read more/,
    })
    expect(links.length).toBeGreaterThan(0)
    expect(links[0]).toHaveAttribute(
      'href',
      '/docs/config#slow-query',
    )
  })

  it('shows sensitive label for secret fields', async () => {
    const user = userEvent.setup()
    render(
      <ConfigTooltip configKey="llm.api_key">
        <span>api key</span>
      </ConfigTooltip>
    )
    await user.hover(screen.getByText('api key'))
    const labels = await screen.findAllByText(/sensitive/i)
    expect(labels.length).toBeGreaterThan(0)
  })

  it('accepts side prop for tooltip placement', () => {
    // Smoke test: passing side="bottom" does not throw.
    render(
      <ConfigTooltip configKey="trust.level" side="bottom">
        <span>placed</span>
      </ConfigTooltip>
    )
    expect(screen.getByText('placed')).toBeInTheDocument()
  })
})
