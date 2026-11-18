import { useEffect, useState } from 'react'
import { approvalShortcutFor } from '../../components/keymap'

// useApprovalKeys runs the approval list's verbs (ux-05a win 24): j and k
// move the selection over count cards, and a, r and s call onVerb with the
// verb and the selected index. Keys are ignored while typing, with a
// modifier held, on a held-down repeat of a verb, or while a dialog is
// open, so nothing acts behind a confirm or the shortcuts sheet.

// modalOpen is true while a native dialog or the shortcuts sheet is open.
function modalOpen() {
  return Boolean(document.querySelector(
    'dialog[open], [role="dialog"][aria-modal="true"]:not(dialog)'))
}

export function useApprovalKeys(count, onVerb, initial = 0) {
  const [selected, setSelected] = useState(initial)
  const [used, setUsed] = useState(false)
  const index = count > 0 ? Math.min(Math.max(selected, 0), count - 1) : -1
  useEffect(() => {
    const onKey = e => {
      const verb = approvalShortcutFor(e)
      if (!verb || count === 0 || e.defaultPrevented || modalOpen()) return
      e.preventDefault()
      setUsed(true)
      if (verb === 'next') setSelected(Math.min(index + 1, count - 1))
      else if (verb === 'previous') setSelected(Math.max(index - 1, 0))
      else if (!e.repeat) onVerb(verb, index)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [count, index, onVerb])
  return { selected: index, used }
}
