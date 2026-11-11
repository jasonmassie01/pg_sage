import { act, fireEvent, screen, within } from '@testing-library/react'

// answerConfirm answers the open ConfirmDialog (dashboard-10) with 'confirm'
// or 'cancel' and returns its title, read before the dialog closes.
export async function answerConfirm(choice = 'confirm') {
  const dialog = await screen.findByRole('dialog')
  const title = within(dialog).getByRole('heading').textContent
  await act(async () => {
    fireEvent.click(within(dialog).getByTestId(`confirm-dialog-${choice}`))
  })
  return title
}
