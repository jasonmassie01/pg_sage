import { describe, expect, it, vi } from 'vitest'
import { FakeEventSource } from './fakeEventSource'

describe('FakeEventSource', () => {
  it('is installed globally and starts each test with no instances', () => {
    expect(globalThis.EventSource).toBe(FakeEventSource)
    expect(FakeEventSource.instances).toHaveLength(0)
    new EventSource('/s')
    expect(FakeEventSource.instances).toHaveLength(1)
  })

  it('was reset after the previous test', () => {
    expect(FakeEventSource.instances).toHaveLength(0)
  })

  it('routes named events to listeners and message to onmessage', () => {
    const es = new EventSource('/s')
    const named = vi.fn()
    const message = vi.fn()
    es.addEventListener('actions', named)
    es.onmessage = message
    es.emit('actions', { type: 'actions' })
    expect(named).toHaveBeenCalledTimes(1)
    expect(named.mock.calls[0][0].data).toBe('{"type":"actions"}')
    expect(message).not.toHaveBeenCalled()
    es.emit('message', 'raw')
    expect(message.mock.calls[0][0].data).toBe('raw')
    es.removeEventListener('actions', named)
    es.emit('actions', {})
    expect(named).toHaveBeenCalledTimes(1)
  })

  it('moves through open, retrying and closed states', () => {
    const es = new EventSource('/s', { withCredentials: true })
    const onerror = vi.fn()
    es.onerror = onerror
    expect(es.withCredentials).toBe(true)
    expect(es.readyState).toBe(FakeEventSource.CONNECTING)
    es.open()
    expect(es.readyState).toBe(FakeEventSource.OPEN)
    es.fail()
    expect(es.readyState).toBe(FakeEventSource.CONNECTING)
    es.fail({ closed: true })
    expect(es.readyState).toBe(FakeEventSource.CLOSED)
    expect(onerror).toHaveBeenCalledTimes(2)
    expect(() => es.emit('actions', {})).toThrow(/closed source/)
    expect(() => es.open()).toThrow(/closed source/)
  })
})
