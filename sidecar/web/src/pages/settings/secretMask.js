// isMaskedSecret reports whether value is the config API's mask for a
// stored secret (all asterisks, or asterisks then at most four characters).
// A masked key is never sent back to the server as if it were the secret.
export function isMaskedSecret(value) {
  if (!value) return false
  const stars = value.match(/^\*+/)?.[0]?.length || 0
  if (stars === 0) return false
  return stars === value.length || value.length - stars <= 4
}
