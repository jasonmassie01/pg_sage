import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { AUTONOMY_CHOICES, choiceFor } from '../../lib/autonomyChoices'
import { DatabaseForm } from './DatabaseForm'

// ux-06b (decision D6): one control, "How much may pg_sage do", with four
// choices; each writes both stored fields. Any other pair is Custom.

const pairs = {
  watch: ['observation', 'manual'], ask: ['advisory', 'approval'],
  tell: ['advisory', 'auto'], window: ['autonomous', 'auto'],
}

describe('choiceFor', () => {
  it('names each of the four pairs and nothing else', () => {
    expect(AUTONOMY_CHOICES.map(c => c.id)).toEqual(['watch', 'ask', 'tell', 'window'])
    for (const [id, [trust, mode]] of Object.entries(pairs)) {
      expect(choiceFor(trust, mode)).toBe(id)
    }
    expect(choiceFor('observation', 'approval')).toBeNull()
    expect(choiceFor('autonomous', 'approval')).toBeNull()
    expect(choiceFor('', '')).toBeNull()
    expect(choiceFor(undefined, undefined)).toBeNull()
    expect(choiceFor('ADVISORY', 'approval')).toBeNull()
  })
})

describe('DatabaseForm autonomy control', () => {
  it('writes both fields for each choice', () => {
    for (const [id, [trust, mode]] of Object.entries(pairs)) {
      const { unmount } = render(<DatabaseForm onClose={vi.fn()} onError={vi.fn()} />)
      if (id !== 'watch') {
        expect(screen.getByTestId(`db-autonomy-${id}`)).not.toBeChecked()
      }
      fireEvent.click(screen.getByTestId(`db-autonomy-${id}`))
      expect(screen.getByTestId(`db-autonomy-${id}`)).toBeChecked()
      expect(screen.getByTestId('db-trust-level')).toHaveValue(trust)
      expect(screen.getByTestId('db-execution-mode')).toHaveValue(mode)
      expect(screen.queryByTestId('db-autonomy-custom')).toBeNull()
      unmount()
    }
  })

  it('starts a new database at Watch only', () => {
    render(<DatabaseForm onClose={vi.fn()} onError={vi.fn()} />)
    expect(screen.getByTestId('db-autonomy-watch')).toBeChecked()
    expect(screen.getByTestId('db-trust-level')).toHaveValue('observation')
    expect(screen.getByTestId('db-execution-mode')).toHaveValue('manual')
  })

  it('shows any other stored pair as Custom, read-only while editing', () => {
    render(<DatabaseForm db={{ id: 7, name: 'prod', trust_level: 'autonomous',
      execution_mode: 'approval' }} onClose={vi.fn()} onError={vi.fn()} />)
    expect(screen.getByTestId('db-autonomy-custom'))
      .toHaveTextContent('Custom: trust_level=autonomous, execution_mode=approval')
    for (const id of Object.keys(pairs)) {
      expect(screen.getByTestId(`db-autonomy-${id}`)).not.toBeChecked()
      expect(screen.getByTestId(`db-autonomy-${id}`)).toBeDisabled()
    }
    expect(screen.getByTestId('db-autonomy-raw')).toHaveTextContent('Edit raw values')
  })

  it('names the stored choice of an edited database', () => {
    render(<DatabaseForm db={{ id: 7, name: 'prod', trust_level: 'advisory',
      execution_mode: 'auto' }} onClose={vi.fn()} onError={vi.fn()} />)
    expect(screen.getByTestId('db-autonomy-tell')).toBeChecked()
  })

  it('lets raw values make a Custom pair on a new database', () => {
    render(<DatabaseForm onClose={vi.fn()} onError={vi.fn()} />)
    fireEvent.change(screen.getByTestId('db-execution-mode'), { target: { value: 'auto' } })
    expect(screen.getByTestId('db-autonomy-custom'))
      .toHaveTextContent('trust_level=observation, execution_mode=auto')
  })
})
