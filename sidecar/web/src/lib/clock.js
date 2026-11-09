// clockTime is a millisecond time as local HH:MM, the way the header and
// the refresh strip name the time data is from.
export function clockTime(ms) {
  const d = new Date(ms)
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
