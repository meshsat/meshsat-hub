// Money is integer minor units end to end (Hub rule 24). This formats them
// for a person with integer arithmetic only: no parseFloat, no toFixed, so a
// figure on a billing page is the figure in the database, never a rounding of it.

// Currencies with no minor unit. Everything else is assumed to have two.
const ZERO_DECIMAL = new Set(['JPY', 'KRW', 'ISK', 'CLP', 'VND', 'XOF', 'XAF'])

export function formatMinor(cents, currency = '') {
  const cur = String(currency || '').toUpperCase()
  const n = Number(cents)
  if (!Number.isFinite(n)) return ''
  const neg = n < 0
  const abs = Math.trunc(Math.abs(n))
  let figure
  if (ZERO_DECIMAL.has(cur)) {
    figure = String(abs)
  } else {
    const whole = Math.trunc(abs / 100)
    const minor = abs % 100
    figure = `${whole}.${String(minor).padStart(2, '0')}`
  }
  return `${neg ? '-' : ''}${figure}${cur ? ' ' + cur : ''}`
}
