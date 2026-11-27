import { intlTimeZone } from '../../lib/time'

// scheduledLabel is when a scheduled approval runs, in the zone the viewer
// chose in the header (local or UTC, ux-05d), or "the next maintenance
// window" when the time is unknown (policy-04).
export function scheduledLabel(row) {
  const at = row?.run_after ? new Date(row.run_after) : null
  if (!at || Number.isNaN(at.getTime())) return 'the next maintenance window'
  return at.toLocaleString(undefined, { weekday: 'short', year: 'numeric', month: 'short',
    day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short',
    timeZone: intlTimeZone() })
}
