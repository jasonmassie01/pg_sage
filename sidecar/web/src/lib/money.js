// formatMoney shows an amount in its ISO 4217 currency ('unknown' when it
// is not a number); a code Intl does not know falls back to plain digits.
export function formatMoney(amount, currency) {
  if (amount === null || amount === undefined || !Number.isFinite(Number(amount))) {
    return 'unknown'
  }
  const code = currency && currency !== 'mixed' ? currency : 'USD'
  try {
    return new Intl.NumberFormat('en-US', { style: 'currency', currency: code })
      .format(Number(amount))
  } catch (err) {
    if (!(err instanceof RangeError)) throw err
    return `${Number(amount).toFixed(2)} ${code}`
  }
}
