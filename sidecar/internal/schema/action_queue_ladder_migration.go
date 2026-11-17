package schema

// ddlActionQueueLadder adds the verification ladder of a queued action
// (lab-03a): the records of what the change was tested on before
// production (plan only, a clone, a canary), appended by each producer
// with one UPDATE so concurrent producers never overwrite each other.
// NULL is a row queued before the ladder existed.
const ddlActionQueueLadder = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.action_queue
    ADD COLUMN IF NOT EXISTS ladder jsonb;
`
