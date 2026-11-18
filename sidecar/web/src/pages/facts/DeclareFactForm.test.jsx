import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DeclareFactForm } from './DeclareFactForm'

// evid-03b: the declare form offers "Do not touch" for tables, indexes and
// schemas; the declaration sends no value fields (an optional note goes in
// the decision note). No concurrency tests: one person fills one form.

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

afterEach(() => vi.unstubAllGlobals())

describe('DeclareFactForm do_not_touch', () => {
  it('offers Do not touch for tables, indexes and schemas and declares it', async () => {
    const posted = []
    vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
      if (init.method === 'POST') {
        posted.push({ url, body: JSON.parse(init.body) })
        return response(201, { fact: { id: 9, summary: 'table public.orders must not be '
          + 'touched', provenance: 'fact #9, confirmed by bob@example.com on 2026-11-15' } })
      }
      return response(200, { objects: ['public.orders'], truncated: false })
    }))
    render(<DeclareFactForm database="orders" />)
    const type = screen.getByTestId('declare-type')
    const option = within(type).getAllByRole('option').find(o => o.value === 'do_not_touch')
    expect(option).toHaveTextContent('Do not touch')
    fireEvent.change(type, { target: { value: 'do_not_touch' } })
    const kinds = within(screen.getByTestId('declare-kind')).getAllByRole('option')
      .map(o => o.value)
    expect(kinds).toEqual(['table', 'index', 'schema'])
    expect(screen.getByTestId('declare-submit')).toBeDisabled()
    fireEvent.change(screen.getByTestId('declare-subject'),
      { target: { value: ' public.orders ' } })
    fireEvent.change(screen.getByTestId('declare-note'), { target: { value: 'owner asked' } })
    fireEvent.click(screen.getByTestId('declare-submit'))
    expect(await screen.findByTestId('declare-success')).toHaveTextContent(
      'table public.orders must not be touched')
    expect(posted).toEqual([{ url: '/api/v1/facts?database=orders', body: {
      type: 'do_not_touch', subject_kind: 'table', subject: 'public.orders', value: {},
      note: 'owner asked' } }])
  })
})
