/**
 * Export an array of objects as a CSV file download.
 * @param {Array<Object>} rows - Data rows
 * @param {string} filename - Download filename (without .csv)
 * @param {Array<string>} [columns] - Column keys to include (default: all keys from first row)
 */
export function exportCSV(rows, filename, columns) {
  if (!rows || rows.length === 0) return

  const cols = columns || Object.keys(rows[0])
  const header = cols.join(',')
  const lines = rows.map(row =>
    cols.map(c => {
      const val = row[c]
      if (val === null || val === undefined) return ''
      const str = String(val)
      // Quote if contains comma, quote, or newline
      if (str.includes(',') || str.includes('"') || str.includes('\n')) {
        return '"' + str.replace(/"/g, '""') + '"'
      }
      return str
    }).join(',')
  )

  const csv = header + '\n' + lines.join('\n')
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${filename}.csv`
  a.click()
  URL.revokeObjectURL(url)
}
