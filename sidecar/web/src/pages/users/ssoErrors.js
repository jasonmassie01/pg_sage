// D7: readable messages for the sso_error codes the OAuth callback puts in
// the dashboard URL. Only known codes map to their text; anything else gets
// the generic message, so the URL can never inject text into the page.

const messages = {
  link_required: 'An account with this email already exists. Sign in with ' +
    'your password and link SSO from your account page, or ask an ' +
    'administrator for an SSO link.',
  link_conflict: 'This SSO identity cannot be linked to this account. It is ' +
    'already linked to another account, this account is already linked, or ' +
    'the verified email does not match the account email.',
  unverified: 'Your identity provider did not confirm your email address, ' +
    'so pg_sage cannot sign you in with it.',
}

const genericMessage = 'Single sign-on failed. Try again, or sign in with ' +
  'your password.'

export function ssoErrorMessage(code) {
  if (!code) return null
  return Object.hasOwn(messages, code) ? messages[code] : genericMessage
}

// replaceHash rewrites the current history entry's hash without adding an
// entry, so values removed from the URL do not linger in history.
export function replaceHash(hash) {
  const { pathname, search } = window.location
  window.history.replaceState(window.history.state, '', `${pathname}${search}${hash}`)
}

// hashPath is the current hash without its query, e.g. '#/login'.
export function hashPath() {
  return (window.location.hash || '').split('?')[0]
}
