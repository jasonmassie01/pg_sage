import { useRef, useState } from 'react'
import { useToast } from '../../components/Toast'
import { SETTINGS_SAVED_EVENT } from '../../components/SecretsPostureBanner'
import { firstRangeProblem } from './valueRange'

// Saving and resetting Settings (dashboard-30). Both writes carry the
// desired generation the page read, so a write over a newer config is a 409:
// the page refetches and keeps the operator's edits. The request bodies are
// pinned by SettingsPage.requests.test.jsx.

const JSON_HEADERS = { 'Content-Type': 'application/json' }

// writeNotice is the banner after a write: the fields that wait for a
// restart (info), else the server's warnings (error), else nothing.
export function writeNotice(result) {
  if (result.pending_restart?.length) {
    return { type: 'info', msg: `Pending restart: ${result.pending_restart.join(', ')}` }
  }
  if (result.warnings?.length) return { type: 'error', msg: result.warnings.join('; ') }
  return null
}

// resetUrl is the DELETE for a stored override of key, or null when the key
// has none this scope may remove (execution_mode is never reset).
export function resetUrl(s, key) {
  const source = s.cfg[key]?.source
  if (key === 'execution_mode') return null
  let base
  if (s.isDatabaseScope && source === 'db_override') {
    base = `/api/v1/config/databases/${s.numericDatabaseId}/${encodeURIComponent(key)}`
  } else if (!s.isDatabaseScope && source === 'override') {
    base = `/api/v1/config/global/${encodeURIComponent(key)}`
  } else {
    return null
  }
  return `${base}?expected_generation=${s.data?.desired_generation ?? ''}`
}

function scopeWord(s) {
  if (s.isDatabaseScope) return 'database '
  return s.isStandaloneScope ? 'instance ' : 'global '
}

function useResetField(s, toast) {
  return async (key) => {
    if (s.readOnly) return
    if (key in s.edits) {
      s.dropEdit(key)
      return
    }
    const url = resetUrl(s, key)
    if (!url) return
    try {
      const res = await fetch(url, {
        method: 'DELETE', credentials: 'include', headers: JSON_HEADERS,
      })
      if (!res.ok) {
        const err = await res.json().catch(() => ({}))
        if (res.status === 409) await s.refetch()
        toast.error(err.error || 'Reset failed')
        return
      }
      s.setFeedback(writeNotice(await res.json().catch(() => ({}))))
      toast.success(s.isDatabaseScope
        ? `Reset ${key} to inherited value`
        : `Reset ${key} to configured default`)
      s.refetch()
    } catch (e) {
      toast.error(e.message || 'Reset failed')
    }
  }
}

// putEdits sends the edits to the scope's endpoint. It returns the error
// message for a failed write, or null after a successful one.
async function putEdits(s) {
  if (!s.configUrl) return 'Selected database is not available'
  const res = await fetch(s.configUrl, {
    method: 'PUT',
    headers: JSON_HEADERS,
    credentials: 'include',
    body: JSON.stringify({ ...s.edits, expected_generation: s.data?.desired_generation }),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({}))
    if (res.status === 409) await s.refetch()
    return err.error || 'Save failed'
  }
  s.setFeedback(writeNotice(await res.json().catch(() => ({}))))
  return null
}

// useSettingsSave is the review-then-apply flow. One save at a time: a
// second Apply while a PUT is in flight is ignored. A failed save stays in
// the open review dialog with its diff.
export function useSettingsSave(s) {
  const toast = useToast()
  const [saving, setSaving] = useState(false)
  const [showDiff, setShowDiff] = useState(false)
  const [saveError, setSaveError] = useState(null)
  const savingRef = useRef(false)
  const failSave = msg => {
    setSaveError(msg)
    toast.error(msg)
  }
  const requestSave = () => {
    if (!s.hasEdits) return
    // config-02b: an out-of-range edit is refused before the review.
    const problem = firstRangeProblem(s.edits)
    if (problem) {
      s.setFeedback({ type: 'error', msg: problem })
      return
    }
    s.setFeedback(null)
    setSaveError(null)
    setShowDiff(true)
  }
  const cancelSave = () => {
    setSaveError(null)
    setShowDiff(false)
  }
  const confirmSave = async () => {
    if (savingRef.current) return
    if (!s.hasEdits) return setShowDiff(false)
    savingRef.current = true
    setSaving(true)
    setSaveError(null)
    try {
      const failure = await putEdits(s)
      if (failure) return failSave(failure)
      const count = Object.keys(s.edits).length
      toast.success(`Saved ${count} ${scopeWord(s)}configuration change(s)`)
      s.discard()
      setShowDiff(false)
      s.refetch()
      window.dispatchEvent(new CustomEvent(SETTINGS_SAVED_EVENT))
    } catch (e) {
      failSave(e.message || 'Save failed')
    } finally {
      savingRef.current = false
      setSaving(false)
    }
  }
  const resetField = useResetField(s, toast)
  return { saving, showDiff, saveError, requestSave, cancelSave, confirmSave, resetField }
}
