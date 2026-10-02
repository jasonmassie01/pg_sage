// checkText is an unmet check's instruction (the server's "how"), or what
// is missing when the server sent none.
export function checkText(c) {
  return c.how || `${c.name}: ${c.observed} (needs ${c.required})`
}
