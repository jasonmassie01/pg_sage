import { useCallback, useRef, useState } from 'react'

// useConfirmTarget holds the state of a ConfirmDialog that names one target
// and runs one request for it (dashboard-10b). Unlike askConfirm, the
// dialog stays open while the request runs and after it fails, so the
// operator reads the error where they confirmed; it closes only on success.
//
// open(target) shows the dialog unless one is already open, so a second row
// cannot replace the target the operator is reading. accept() runs
// perform(target) once, however often it is pressed while in flight, then
// calls onDone(target, result). cancel() closes, except while in flight.
// error is an Error, or null.
export function useConfirmTarget({ perform, onDone }) {
  const [target, setTarget] = useState(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const inFlight = useRef(false)
  const shown = useRef(null)

  const show = useCallback(next => {
    shown.current = next
    setTarget(next)
    setError(null)
  }, [])

  const open = useCallback(next => {
    if (shown.current === null) show(next)
  }, [show])

  const cancel = useCallback(() => {
    if (!inFlight.current) show(null)
  }, [show])

  async function accept() {
    if (inFlight.current || target === null) return
    inFlight.current = true
    setBusy(true)
    setError(null)
    try {
      const result = await perform(target)
      show(null)
      if (onDone) onDone(target, result)
    } catch (err) {
      setError(err instanceof Error ? err : new Error(String(err)))
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }

  return { target, busy, error, open, cancel, accept }
}
