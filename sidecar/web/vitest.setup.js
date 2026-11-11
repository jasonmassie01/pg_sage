// vitest.setup.js — test environment bootstrap for Vitest.
//
// Loads @testing-library/jest-dom custom matchers (toBeInTheDocument,
// toHaveAttribute, ...) and the FakeEventSource. Run once before the
// test suite.
//
// jsdom does not implement ResizeObserver or DOMRect, both of which
// Radix uses internally for positioning. Polyfill with no-op stubs so
// the tooltip can render in tests without exercising layout math.

import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'
import { FakeEventSource } from './src/test/fakeEventSource'

// jsdom has no EventSource. Install the fake for every test so the real
// LiveEventsProvider can run, and forget its instances (and any stub a
// test left in its place) after each test.
globalThis.EventSource = FakeEventSource
afterEach(() => {
  FakeEventSource.reset()
  globalThis.EventSource = FakeEventSource
})

class NoopResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver = globalThis.ResizeObserver || NoopResizeObserver

if (!globalThis.DOMRect) {
  globalThis.DOMRect = class DOMRect {
    constructor(x = 0, y = 0, width = 0, height = 0) {
      this.x = x
      this.y = y
      this.width = width
      this.height = height
      this.top = y
      this.left = x
      this.right = x + width
      this.bottom = y + height
    }
  }
}

// dashboard-10: jsdom has HTMLDialogElement but neither showModal nor close.
// The polyfill sets and clears the open attribute and fires close like a
// browser. It does not emulate the top layer, inertness or Escape: Dialog
// handles Escape itself, and focus containment is a MANUAL browser check.
if (globalThis.HTMLDialogElement
  && !globalThis.HTMLDialogElement.prototype.showModal) {
  const proto = globalThis.HTMLDialogElement.prototype
  proto.showModal = function showModal() {
    if (!this.isConnected) {
      throw new DOMException('showModal on a disconnected dialog', 'InvalidStateError')
    }
    this.setAttribute('open', '')
  }
  proto.show = function show() { this.setAttribute('open', '') }
  proto.close = function close(returnValue) {
    if (!this.hasAttribute('open')) return
    if (returnValue !== undefined) this.returnValue = returnValue
    this.removeAttribute('open')
    this.dispatchEvent(new Event('close'))
  }
}
