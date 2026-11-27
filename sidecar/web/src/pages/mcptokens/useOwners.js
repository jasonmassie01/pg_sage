// The users who may own an operator token (operators and admins), loaded
// once for the create form's owner picker and the list's Owner column.
import { useEffect, useState } from 'react'
import { listOwners } from './tokenApi'

export function useOwners() {
  const [state, setState] = useState({ owners: null, error: null })
  useEffect(() => {
    let live = true
    listOwners()
      .then(owners => { if (live) setState({ owners, error: null }) })
      .catch(err => { if (live) setState({ owners: null, error: err.message }) })
    return () => { live = false }
  }, [])
  return state
}
