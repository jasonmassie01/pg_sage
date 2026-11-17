package schema

// ddlUserInboxSeenAt (ux-01b) keeps the time a person last saw the Inbox
// on their user row, so "Since you were last here" follows them across
// devices. NULL is a first visit. POST /api/v1/inbox/seen moves it forward
// only (GREATEST). ADD COLUMN IF NOT EXISTS of a nullable column with no
// default takes no rewrite; lock_timeout bounds the first run's wait.
const ddlUserInboxSeenAt = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.users ADD COLUMN IF NOT EXISTS inbox_seen_at timestamptz;
`
