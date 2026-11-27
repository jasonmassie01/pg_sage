import { useEffect, useState } from 'react'
import { requestJSON } from './parts'

// doctor: the Connect step's read of GET /api/v1/doctor (ux-04a, ux-04b).

// useDoctor reads the report once per database: { loading, report, error }.
export function useDoctor(database) {
  const [state, setState] = useState({ loading: true, report: null, error: null })
  useEffect(() => {
    let live = true
    const url = `/api/v1/doctor?database=${encodeURIComponent(database)}`
    requestJSON(url).then(({ ok, status, body }) => {
      if (!live) return
      if (ok) setState({ loading: false, report: body, error: null })
      else setState({ loading: false, report: null, error: body?.error || `HTTP ${status}` })
    }, err => {
      if (live) setState({ loading: false, report: null, error: err.message })
    })
    return () => { live = false }
  }, [database])
  return state
}

// connectionFailed: the doctor says pg_sage cannot reach the database.
export function connectionFailed(report) {
  return Boolean(report?.checks?.some(c => c.id === 'connection' && c.status === 'fail'))
}
