// FakeEventSource stands in for the browser EventSource, which jsdom
// does not implement. vitest.setup.js installs it on globalThis and
// clears `instances` after every test, so a test drives the real
// LiveEventsProvider instead of mocking it away.
//
// A test reads FakeEventSource.instances to find the source the code
// under test opened, then calls open(), emit(type, data) and
// fail({closed}) the way a server and the browser would.
export class FakeEventSource {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSED = 2
  static instances = []

  constructor(url, init = {}) {
    this.url = String(url)
    this.withCredentials = Boolean(init.withCredentials)
    this.readyState = FakeEventSource.CONNECTING
    this.onopen = null
    this.onmessage = null
    this.onerror = null
    this.closeCalls = 0
    this.listeners = new Map()
    FakeEventSource.instances.push(this)
  }

  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set())
    this.listeners.get(type).add(fn)
  }

  removeEventListener(type, fn) {
    this.listeners.get(type)?.delete(fn)
  }

  close() {
    this.closeCalls += 1
    this.readyState = FakeEventSource.CLOSED
  }

  // open simulates the server accepting the stream.
  open() {
    this.#assertNotClosed('open')
    this.readyState = FakeEventSource.OPEN
    this.#fire('open', new Event('open'))
  }

  // emit delivers one server event. Like the browser, an event named
  // "message" reaches onmessage and its listeners; any other name
  // reaches only the listeners added for that name. A non-string data
  // value is JSON-encoded, the way the sidecar writes its data lines.
  emit(type, data) {
    this.#assertNotClosed('emit')
    const payload = typeof data === 'string' ? data : JSON.stringify(data)
    this.#fire(type, new MessageEvent(type, { data: payload }))
  }

  // fail simulates a dropped stream. With closed false the browser is
  // retrying (CONNECTING); with closed true it gave up (CLOSED), which
  // is what a non-200 reconnect answer does per the EventSource spec.
  fail({ closed = false } = {}) {
    this.readyState = closed ? FakeEventSource.CLOSED : FakeEventSource.CONNECTING
    this.#fire('error', new Event('error'))
  }

  // reset forgets every instance; called from vitest.setup.js.
  static reset() {
    FakeEventSource.instances.length = 0
  }

  #fire(type, evt) {
    const handler = this[`on${type}`]
    if (typeof handler === 'function') handler.call(this, evt)
    for (const fn of [...(this.listeners.get(type) || [])]) fn.call(this, evt)
  }

  #assertNotClosed(what) {
    if (this.readyState === FakeEventSource.CLOSED) {
      throw new Error(`FakeEventSource: ${what}() on a closed source (${this.url})`)
    }
  }
}
