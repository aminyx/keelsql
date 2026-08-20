package keelsql

import (
	"strings"
	"unicode/utf8"

	"github.com/aminyx/keelsql/types"
)

// FormatTable renders a result as an ASCII table:
//
//	+----+-------+
//	| id | name  |
//	+----+-------+
//	| 1  | ada   |
//	| 2  | grace |
//	+----+-------+
//
// It lives in the library rather than in the CLI so that the formatting is
// covered by the same tests as everything else. Column widths are measured
// in runes, so a non-ASCII value still lines up.
func FormatTable(columns []string, rows [][]types.Value) string {
	if len(columns) == 0 {
		return ""
	}
	cells := make([][]string, len(rows))
	widths := make([]int, len(columns))
	for i, name := range columns {
		widths[i] = utf8.RuneCountInString(name)
	}
	for r, row := range rows {
		cells[r] = make([]string, len(columns))
		for c := range columns {
			text := ""
			if c < len(row) {
				text = row[c].String()
			}
			cells[r][c] = text
			if w := utf8.RuneCountInString(text); w > widths[c] {
				widths[c] = w
			}
		}
	}

	var sb strings.Builder
	rule := border(widths)
	sb.WriteString(rule)
	writeRow(&sb, columns, widths)
	sb.WriteString(rule)
	for _, row := range cells {
		writeRow(&sb, row, widths)
	}
	sb.WriteString(rule)
	return sb.String()
}

func border(widths []int) string {
	var sb strings.Builder
	sb.WriteByte('+')
	for _, w := range widths {
		sb.WriteString(strings.Repeat("-", w+2))
		sb.WriteByte('+')
	}
	sb.WriteByte('\n')
	return sb.String()
}

func writeRow(sb *strings.Builder, cells []string, widths []int) {
	sb.WriteByte('|')
	for i, w := range widths {
		text := ""
		if i < len(cells) {
			text = cells[i]
		}
		sb.WriteByte(' ')
		sb.WriteString(text)
		sb.WriteString(strings.Repeat(" ", w-utf8.RuneCountInString(text)))
		sb.WriteString(" |")
	}
	sb.WriteByte('\n')
}
