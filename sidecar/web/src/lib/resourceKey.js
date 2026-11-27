// resourceKey identifies what a URL fetches, ignoring the from/to time
// window. Moving the window (the 30 s time-range tick) refreshes the
// same resource, so its data stays visible while revalidating (G9-B05).
export function resourceKey(url) {
  if (!url) return null
  const [path, query = ''] = url.split('?')
  const params = new URLSearchParams(query)
  params.delete('from')
  params.delete('to')
  const rest = params.toString()
  return rest ? `${path}?${rest}` : path
}
