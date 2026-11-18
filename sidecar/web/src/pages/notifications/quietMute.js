// ux-09b: the URLs and choices of the channel quiet hours, the mute
// dialog and the weekly line.

export const MUTES_URL = '/api/v1/notifications/mutes'
export const WEEKLY_LINE_URL = '/api/v1/notifications/weekly-line'

export function quietHoursURL(channelID) {
  return `/api/v1/notifications/channels/${channelID}/quiet-hours`
}

// A mute lasts at most 90 days (the API refuses longer).
export const MUTE_DURATIONS = [
  { label: '1 hour', hours: 1 },
  { label: '8 hours', hours: 8 },
  { label: '24 hours', hours: 24 },
  { label: '7 days', hours: 24 * 7 },
  { label: '30 days', hours: 24 * 30 },
  { label: '90 days', hours: 24 * 90 },
]

export const MUTE_CLASSES = [
  { value: '', label: 'Every class' },
  { value: 'page', label: 'Page' },
  { value: 'ask', label: 'Ask' },
  { value: 'digest', label: 'Digest' },
]

// muteUntil is the RFC 3339 end of a mute of hours from now.
export function muteUntil(hours, now = new Date()) {
  const until = new Date(now.getTime() + hours * 3600 * 1000)
  return until.toISOString().replace(/\.\d{3}Z$/, 'Z')
}

// classLabel names a mute's class for the list.
export function classLabel(value) {
  return (MUTE_CLASSES.find(c => c.value === (value || '')) || MUTE_CLASSES[0]).label
}

// apiError is the API's error text, or fallback.
export async function apiError(res, fallback) {
  const d = await res.json().catch(() => ({}))
  return d.error || fallback
}
