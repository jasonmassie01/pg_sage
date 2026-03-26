import { useState, useEffect, useCallback } from 'react'

export function useAPI(url, interval = 30000) {
  const [data, setData] = useState(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)

  const fetchData = useCallback(async () => {
    if (!url) {
      setLoading(false)
      return
    }
    try {
      const res = await fetch(url)
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`)
      const json = await res.json()
      setData(json)
      setError(null)
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [url])

  useEffect(() => {
    fetchData()
    if (interval > 0 && url) {
      const id = setInterval(fetchData, interval)
      return () => clearInterval(id)
    }
  }, [fetchData, interval, url])

  return { data, loading, error, refetch: fetchData }
}
