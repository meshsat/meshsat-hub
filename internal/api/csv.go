package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
)

// writeCSV writes rows as a CSV file response with appropriate headers.
func writeCSV(w http.ResponseWriter, filename string, headers []string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	cw := csv.NewWriter(w)
	_ = cw.Write(headers)
	for _, row := range rows {
		_ = cw.Write(neutraliseCSVRow(row))
	}
	cw.Flush()
}

// neutraliseCSVRow defuses CSV injection: a cell that starts with = + - @ or a
// tab/CR is a formula to Excel and LibreOffice, and every value here is
// tenant-supplied (a device label, an audit detail, a contact name), so an
// operator opening an export must not be the one who runs it. The cell is
// prefixed with a single quote, which spreadsheets show as text; a leading
// minus in a plain number (coordinates, balances) is kept because a cell that
// parses as a number is not a formula.
func neutraliseCSVRow(row []string) []string {
	out := make([]string, len(row))
	for i, c := range row {
		out[i] = neutraliseCSVCell(c)
	}
	return out
}

func neutraliseCSVCell(c string) string {
	if c == "" {
		return c
	}
	switch c[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + c
	case '-':
		if _, err := strconv.ParseFloat(c, 64); err == nil {
			return c
		}
		return "'" + c
	}
	return c
}
