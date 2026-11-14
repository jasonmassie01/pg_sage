import { useEffect } from 'react'

// alertsTarget is the Notifications Log tab URL for an /alerts hash,
// keeping its ?database= (alerting-06c).
function alertsTarget(route) {
  const database = new URLSearchParams(String(route ?? '').split('?')[1] || '')
    .get('database')
  const params = new URLSearchParams({ tab: 'log' })
  if (database) params.set('database', database)
  return `#/notifications?${params.toString()}`
}

// AlertsRedirect sends the old Alert log URL to the Notifications Log
// tab. It replaces the history entry, so Back skips /alerts instead of
// bouncing through it, and announces the change like a hash navigation.
export function AlertsRedirect({ route }) {
  useEffect(() => {
    window.history.replaceState(window.history.state, '', alertsTarget(route))
    window.dispatchEvent(new HashChangeEvent('hashchange'))
  }, [route])
  return (
    <p data-testid="alerts-redirect" className="text-sm"
      style={{ color: 'var(--text-secondary)' }}>
      The Alert log moved to Notifications · Log.
    </p>
  )
}
