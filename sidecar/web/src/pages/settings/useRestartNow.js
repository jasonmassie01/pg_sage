import { useState } from 'react'
import { useToast } from '../../components/Toast'
import { useConfirm } from '../../hooks/useConfirm'

// useRestartNow asks before restarting the sidecar so startup-only settings
// take effect, then reloads the page once it is back. dialog is the confirm
// dialog element, rendered next to the restart control.
export function useRestartNow() {
  const toast = useToast()
  const [restarting, setRestarting] = useState(false)
  const { askConfirm, dialog } = useConfirm()
  const restartNow = async () => {
    const ok = await askConfirm({
      title: 'Restart the pg_sage sidecar now?', action: 'Restart',
      consequence: 'pg_sage is unavailable for a few seconds while startup-only '
        + 'settings take effect.',
    })
    if (!ok) return
    setRestarting(true)
    try {
      const res = await fetch('/api/v1/restart', {
        method: 'POST',
        credentials: 'include',
        // requireJSONMiddleware rejects POSTs without it (G9-B03).
        headers: { 'Content-Type': 'application/json' },
      })
      if (res.status === 501) {
        toast.error('Restart not supported: no supervisor configured')
        return
      }
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        toast.error(err.error || 'Restart failed')
        return
      }
      toast.success('Restarting — reconnecting in a few seconds…')
      setTimeout(() => window.location.reload(), 6000)
    } catch (e) {
      toast.error(e.message || 'Restart failed')
    } finally {
      setRestarting(false)
    }
  }
  return { restarting, restartNow, dialog }
}
