// useAskSession.js — request state for one database's Ask Sage session:
// the thread, the current conversation, in-flight and error state, the
// active asynchronous run (useAskRun), plus the budget and
// conversation-list resources refreshed after each answer.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  budgetURL, conversationURL, conversationsURL, describeAskError, runsURL,
} from '../../lib/ask'
import { useAskRun } from './useAskRun'

// requestJSON sends a session-authenticated request. A non-JSON body is
// tolerated (null); HTTP errors come back as {ok: false, error}; network
// failures reject with the fetch TypeError.
export async function requestJSON(url, init = {}) {
  const res = await fetch(url, { credentials: 'include', ...init })
  let body = null
  try {
    body = await res.json()
  } catch (err) {
    if (!(err instanceof SyntaxError)) throw err
  }
  if (!res.ok) return { ok: false, error: describeAskError(res.status, body) }
  return { ok: true, data: body }
}

// useAskResource GETs url whenever url or tick changes.
export function useAskResource(url, tick, onDisabled) {
  const [state, setState] = useState({ data: null, error: null })
  useEffect(() => {
    let live = true
    requestJSON(url).then(r => {
      if (!live) return
      if (r.ok) {
        setState({ data: r.data, error: null })
        return
      }
      if (r.error.kind === 'disabled') onDisabled()
      setState({ data: null, error: r.error.message })
    }).catch(err => {
      // Surfaced to the page (budget hides, the list shows the message).
      if (live) setState({ data: null, error: err.message })
    })
    return () => { live = false }
  }, [url, tick, onDisabled])
  return state
}

const postInit = body => ({
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
})

// useAsk starts a run of the question in the current conversation and
// resolves when it ends (true with an answer). busyRef guards against a
// second submit before React re-renders the disabled button. readOnly
// sends read_only: the server then offers no write tool.
function useAsk(database, conv, startRun, readOnly) {
  const [busy, setBusy] = useState(false)
  const busyRef = useRef(false)
  const ask = useCallback(async question => {
    if (busyRef.current) return false
    busyRef.current = true
    setBusy(true)
    conv.clearErrors()
    const body = conv.id ? { question, conversation_id: conv.id } : { question }
    if (readOnly) body.read_only = true
    try {
      const r = await requestJSON(runsURL(database), postInit(body))
      if (!r.ok) {
        conv.fail(r.error)
        return false
      }
      if (typeof r.data?.run_id !== 'string' || !r.data.run_id) {
        conv.fail({ kind: 'http', message: 'The server did not start the question.' })
        return false
      }
      if (r.data.conversation_id) conv.setId(r.data.conversation_id)
      return await startRun(r.data.run_id)
    } catch (err) {
      if (!(err instanceof TypeError)) throw err
      conv.setNetError({ message: err.message, question })
      return false
    } finally {
      busyRef.current = false
      setBusy(false)
    }
  }, [database, conv, startRun, readOnly])
  return { busy, ask }
}

// useConversation holds the thread, the current conversation id and the
// error state, and loads a stored conversation into the thread.
function useConversation(database, onDisabled) {
  const [thread, setThread] = useState([])
  const [id, setId] = useState(null)
  const [error, setError] = useState(null)
  const [netError, setNetError] = useState(null)
  const fail = useCallback(e => (e.kind === 'disabled' ? onDisabled() : setError(e)),
    [onDisabled])
  const clearErrors = useCallback(() => {
    setError(null)
    setNetError(null)
  }, [])
  const load = useCallback(async convId => {
    clearErrors()
    try {
      const r = await requestJSON(conversationURL(database, convId))
      if (!r.ok) {
        fail(r.error)
        return
      }
      setThread(Array.isArray(r.data?.answers) ? r.data.answers : [])
      setId(convId)
    } catch (err) {
      if (!(err instanceof TypeError)) throw err
      setError({ kind: 'network', message: err.message })
    }
  }, [database, fail, clearErrors])
  const reset = useCallback(() => {
    setThread([])
    setId(null)
    clearErrors()
  }, [clearErrors])
  return useMemo(() => ({ thread, setThread, id, setId, error, netError, setNetError,
    fail, clearErrors, load, reset }),
  [thread, id, error, netError, fail, clearErrors, load, reset])
}

// useAskSession is one database's session. With { readOnly: true } every
// question is sent answer-only (Explain a case).
export function useAskSession(database, { readOnly = false } = {}) {
  const [disabled, setDisabled] = useState(false)
  const [tick, setTick] = useState(0)
  const markDisabled = useCallback(() => setDisabled(true), [])
  const conv = useConversation(database, markDisabled)
  const { setThread, setId } = conv
  const onAnswer = useCallback(answer => {
    setThread(t => [...t, answer])
    if (answer?.conversation_id) setId(answer.conversation_id)
    setTick(n => n + 1)
  }, [setThread, setId])
  const runner = useAskRun(database, requestJSON, { onAnswer, onError: conv.fail })
  const { busy, ask } = useAsk(database, conv, runner.start, readOnly)
  const budget = useAskResource(budgetURL(database), tick, markDisabled)
  const conversations = useAskResource(conversationsURL(database), tick, markDisabled)
  return {
    thread: conv.thread, conversationId: conv.id, error: conv.error,
    netError: conv.netError, load: conv.load, reset: conv.reset,
    busy: busy || Boolean(runner.run), ask, disabled, budget, conversations,
    run: runner.run, cancelRun: runner.cancel,
  }
}
