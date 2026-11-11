import { useCallback, useEffect, useRef, useState } from 'react'
import { ConfirmDialog } from '../components/ConfirmDialog'

// useConfirm replaces window.confirm with the shared ConfirmDialog
// (dashboard-10). askConfirm(options) opens the dialog and resolves true on
// Confirm, false on Cancel, Escape, a newer request or unmount. Render
// `dialog` once in the owning component. options are ConfirmDialog props.
export function useConfirm() {
  const [request, setRequest] = useState({ id: 0, options: null })
  const pending = useRef(null)

  const settle = useCallback(value => {
    const resolve = pending.current
    pending.current = null
    setRequest(r => ({ ...r, options: null }))
    if (resolve) resolve(value)
  }, [])

  const askConfirm = useCallback(next => new Promise(resolve => {
    if (pending.current) pending.current(false)
    pending.current = resolve
    // A new id remounts the dialog, so a replaced request leaves no typed text.
    setRequest(r => ({ id: r.id + 1, options: next }))
  }), [])

  useEffect(() => () => {
    if (pending.current) pending.current(false)
    pending.current = null
  }, [])

  const { id, options } = request
  const dialog = (
    <ConfirmDialog key={id} {...(options || {})} open={Boolean(options)}
      onConfirm={() => settle(true)} onCancel={() => settle(false)} />
  )
  return { askConfirm, dialog }
}
