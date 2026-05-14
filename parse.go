package main

import (
	"regexp"
	"strings"
)

func parseSpan(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 1
	}
	n := 0

	for _, c := range m[1] {
		if c < '0' || c > '9' {
			return 1
		}
		n = n*10 + int(c-'0')
		if n > 100000 { //prevent overflow by malformed colspan/rowspan
			return 1
		}
	}

	if n < 1 {
		return 1
	}
	return n
}

var (
	reHTMLOpen  = regexp.MustCompile(`(?i)<table\b`)
	reHTMLClose = regexp.MustCompile(`(?i)</table\s*>`)
)

var (
	reTR      = regexp.MustCompile(`(?is)<tr\b.*?</tr>`)
	reTDTH    = regexp.MustCompile(`(?is)<t[dh]\b.*?</t[dh]>`)
	reColspan = regexp.MustCompile(`(?i)\bcolspan\s*=\s*["']?(\d+)`)
	reRowspan = regexp.MustCompile(`(?i)\browspan\s*=\s*["']?(\d+)`)
)

// extractTables returns all wiki-syntax and HTML tables found in text
func extractTables(text string) []string {
	var out []string
	wikiRanges := extractWikiTableRanges(text)
	for _, r := range wikiRanges {
		out = append(out, strings.Clone(text[r[0]:r[1]])) //Only retain table, allows text to be freed later
	}
	out = append(out, extractHTMLTables(text, wikiRanges)...)
	return out
}

// extractWikiTableRanges returns the [start, end) byte ranges of every outermost {| ... |} block
func extractWikiTableRanges(text string) [][2]int {
	if !strings.Contains(text, "{|") {
		return nil
	}

	var out [][2]int
	n := len(text)
	i := 0
	for i < n {
		rel := strings.Index(text[i:], "{|")
		if rel == -1 {
			break
		}
		start := i + rel
		depth, j := 0, start
		tmplDepth := 0
		for j < n {
			if j+2 <= n && text[j:j+2] == "{{" {
				tmplDepth++
				j += 2
				continue
			}
			if j+2 <= n && text[j:j+2] == "}}" {
				if tmplDepth > 0 {
					tmplDepth--
				}
				j += 2
				continue
			}
			if tmplDepth == 0 {
				if j+2 <= n && text[j:j+2] == "{|" {
					depth++
					j += 2
					continue
				}
				if j+2 <= n && text[j:j+2] == "|}" {
					depth--
					j += 2
					if depth == 0 {
						out = append(out, [2]int{start, j})
						break
					}
					continue
				}
			}
			j++
		}
		next := j
		if next <= start+2 {
			next = start + 2
		}
		i = next
	}
	return out
}

// extractHTMLTables returns all <table>...</table> blocks
func extractHTMLTables(text string, wikiRanges [][2]int) []string {
	if !strings.Contains(strings.ToLower(text), "<table") {
		return nil
	}

	inWikiRange := func(pos int) bool {
		for _, r := range wikiRanges {
			if pos >= r[0] && pos < r[1] {
				return true
			}
		}
		return false
	}

	var out []string
	pos := 0
	for {
		mOpen := reHTMLOpen.FindStringIndex(text[pos:])
		if mOpen == nil {
			break
		}
		start := pos + mOpen[0]
		// Skip HTML tables whose opening tag falls inside a wikitable block.
		if inWikiRange(start) {
			pos = start + 1
			continue
		}
		depth := 1
		cur := pos + mOpen[1]
		for depth > 0 && cur < len(text) {
			mo := reHTMLOpen.FindStringIndex(text[cur:])
			mc := reHTMLClose.FindStringIndex(text[cur:])
			if mc == nil {
				break // unclosed table — skip
			}
			if mo != nil && mo[0] < mc[0] {
				// another <table> opens before the next </table>
				depth++
				cur += mo[1]
			} else {
				depth--
				cur += mc[1]
			}
		}

		if depth == 0 {
			table := text[start:cur]
			gt := strings.IndexByte(table, '>') // end of the opening <table ...> tag
			// Extract only leaf tables (no nested <table> tags, no wikitable markup)
			// avoids garbled rows from reTR/reTDTH matching across nested table boundaries
			hasInnerHTML := gt >= 0 && reHTMLOpen.MatchString(table[gt+1:])
			hasWikiTable := strings.Contains(table, "{|")
			if !hasInnerHTML && !hasWikiTable {
				out = append(out, strings.Clone(table))
				pos = cur
			} else if gt >= 0 {
				pos = start + gt + 1 // step inside: just past the opening <table ...> tag
			} else {
				pos = cur
			}
		} else {
			// Unclosed table — advance past the opening tag to avoid re-scanning.
			next := cur
			if next <= start+1 {
				next = start + 1
			}
			pos = next
		}
	}
	return out
}

// parseWikiRawRows parses wikitext into raw rows, handling standard wikitext rows (|- | !) and
// template-based rows ({{TemplateName|p1|p2}} single or multiline)
// Bare | cells following a template row are appended to that row.
func parseWikiRawRows(text string) [][]rawCell {
	var rawRows [][]rawCell
	var current []rawCell

	// template accumulation state
	inTemplate := false
	tmplDepth := 0
	var tmplBuf strings.Builder
	inCellTemplate := false
	cellDepth := 0

	seenOuterOpen := false // true after the outer {| opening line is consumed
	innerDepth := 0        // depth of block-level nested tables
	inCellTable := false   // accumulating an inline nested wikitable into a cell
	cellTableDepth := 0    // {| depth of the current inline nested table

	// flush commits current (the in-progress row) to rawRows and resets it
	flush := func() {
		if current == nil {
			return
		}
		var filtered []rawCell
		for _, c := range current {
			if strings.TrimSpace(c.content) != "" {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) > 0 {
			rawRows = append(rawRows, filtered)
		}
		current = nil
	}

	// applyTemplate turns a single {{Template|p1|p2|…}} into a new row of cells
	// The template name (before the first pipe) is intentionally discarded,
	// so only the parameter values become cells.
	applyTemplate := func(raw string) {
		// raw is everything inside {{ ... }} exclusive
		pipe := strings.IndexByte(raw, '|')
		if pipe < 0 {
			return // no params — template is not a data row
		}
		params := splitTemplateParams(raw[pipe+1:])
		if len(params) == 0 {
			return
		}
		flush()
		current = make([]rawCell, 0, len(params))
		for _, p := range params {
			current = append(current, rawCell{content: p, colspan: 1, rowspan: 1})
		}
		// do NOT flush — bare | lines may follow and get appended
	}

	// countDepth counts {{ and }} depth changes in s.
	countDepth := func(s string, startDepth int) (depth int, closed bool) {
		depth = startDepth
		for i := 0; i < len(s); i++ {
			if i+1 < len(s) && s[i] == '{' && s[i+1] == '{' {
				depth++
				i++
			} else if i+1 < len(s) && s[i] == '}' && s[i+1] == '}' {
				depth--
				i++
				if depth == 0 {
					return 0, true
				}
			}
		}
		return depth, false
	}

	// countTableDepth counts {| and |} depth changes in s.
	countTableDepth := func(s string, startDepth int) (depth int, closed bool) {
		depth = startDepth
		for i := 0; i < len(s); i++ {
			if i+1 < len(s) && s[i] == '{' && s[i+1] == '|' {
				depth++
				i++
			} else if i+1 < len(s) && s[i] == '|' && s[i+1] == '}' {
				depth--
				i++
				if depth == 0 {
					return 0, true
				}
			}
		}
		return depth, false
	}

	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)

		// inside a multiline template
		if inTemplate {
			tmplBuf.WriteByte('\n')
			tmplBuf.WriteString(line)
			newDepth, closed := countDepth(line, tmplDepth)
			if closed {
				inTemplate = false
				raw := tmplBuf.String()
				// extract inner content between {{ and last }}
				if start := strings.Index(raw, "{{"); start >= 0 {
					inner := raw[start+2:]
					if end := strings.LastIndex(inner, "}}"); end >= 0 {
						inner = inner[:end]
					}
					applyTemplate(inner)
				}
			} else {
				tmplDepth = newDepth
			}
			continue
		}
		if inCellTemplate {
			// A cell whose content contains an unclosed {{ }} spans multiple lines.
			// Accumulate each line into the last cell of the current row until template closes or new row marker (|-)
			if strings.HasPrefix(line, "|-") {
				inCellTemplate = false
				cellDepth = 0
				flush()
				current = []rawCell{}
				continue
			}
			if len(current) > 0 {
				last := &current[len(current)-1]
				last.content += "\n" + line
			}
			newDepth, closed := countDepth(line, cellDepth)
			if closed {
				inCellTemplate = false
				cellDepth = 0
			} else {
				cellDepth = newDepth
			}
			continue
		}

		// inside an inline nested wikitable (cell content)
		// Accumulate every line into the last cell until the matching |} closes the nested table.
		// The entire nested table becomes opaque cell content.
		if inCellTable {
			if len(current) > 0 {
				last := &current[len(current)-1]
				last.content += "\n" + line
			}
			newDepth, closed := countTableDepth(line, cellTableDepth)
			if closed {
				inCellTable = false
				cellTableDepth = 0
			} else {
				cellTableDepth = newDepth
			}
			continue
		}

		// normal line processing
		if line == "" || strings.HasPrefix(line, "|+") || strings.HasPrefix(line, "<!--") {
			continue
		}

		// {| outer opening (consumed once) or block-level nested table start.
		if strings.HasPrefix(line, "{|") {
			if !seenOuterOpen {
				seenOuterOpen = true
			} else {
				innerDepth++
			}
			continue
		}

		// |} — close a block-level nested table, or the outer table (skip both).
		if line == "|}" {
			if innerDepth > 0 {
				innerDepth--
			}
			continue
		}

		// Inside a block-level nested table: skip all content.
		if innerDepth > 0 {
			continue
		}

		if strings.HasPrefix(line, "|-") {
			flush()
			current = []rawCell{}
			continue
		}
		if strings.HasPrefix(line, "!") {
			if current == nil {
				current = []rawCell{}
			}
			for _, p := range strings.Split(line[1:], "!!") {
				current = append(current, parseWikiCell(strings.TrimSpace(p)))
			}
			continue
		}
		if strings.HasPrefix(line, "|") {
			if current == nil {
				current = []rawCell{}
			}
			for _, p := range strings.Split(line[1:], "||") {
				current = append(current, parseWikiCell(strings.TrimSpace(p)))
			}
			// If the last cell opens an inline wikitable without closing it, enter inCellTable mode so subsequent lines
			// are accumulated into that cell rather than parsed as outer-table rows.
			if len(current) > 0 {
				d, closed := countTableDepth(current[len(current)-1].content, 0)
				if !closed && d > 0 {
					inCellTable = true
					cellTableDepth = d
					continue
				}
			}
			// If the last cell's content opens a template without closing it,
			// enter inCellTemplate mode so subsequent lines are appended to it.
			if len(current) > 0 {
				d, closed := countDepth(current[len(current)-1].content, 0)
				if !closed && d > 0 {
					inCellTemplate = true
					cellDepth = d
				}
			}
			continue
		}
		// template row
		if strings.HasPrefix(line, "{{") {
			newDepth, closed := countDepth(line, 0)
			if closed {
				// single-line template
				inner := line[2:]
				if end := strings.LastIndex(inner, "}}"); end >= 0 {
					inner = inner[:end]
				}
				applyTemplate(inner)
			} else {
				// multiline template
				flush()
				inTemplate = true
				tmplDepth = newDepth
				tmplBuf.Reset()
				tmplBuf.WriteString(line)
			}
			continue
		}
		// continuation of current cell content
		if current != nil && len(current) > 0 {
			last := current[len(current)-1]
			last.content += "\n" + line
			current[len(current)-1] = last
		}
	}

	if inTemplate {
		// unclosed template at end
		raw := tmplBuf.String()
		if start := strings.Index(raw, "{{"); start >= 0 {
			inner := raw[start+2:]
			applyTemplate(inner)
		}
	}
	flush()
	return rawRows
}

// tableToRawRows dispatches to the HTML or wiki parser based on the table
func tableToRawRows(text string) [][]rawCell {
	if text == "" {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(text), "{|") {
		return parseWikiRawRows(text)
	}
	return htmlToRawRows(text)
}

// htmlToRawRows extracts rows from an HTML table
// Without <tr> blocks the entire text is treated as a single row
func htmlToRawRows(text string) [][]rawCell {
	var rawRows [][]rawCell
	blocks := reTR.FindAllString(text, -1)
	if blocks == nil {
		blocks = []string{text}
	}
	for _, b := range blocks {
		var row []rawCell
		for _, c := range reTDTH.FindAllString(b, -1) {
			row = append(row, rawCell{
				content: strings.TrimSpace(c),
				colspan: parseSpan(reColspan, c),
				rowspan: parseSpan(reRowspan, c),
			})
		}
		if len(row) > 0 {
			rawRows = append(rawRows, row)
		}
	}
	return rawRows
}

func parseWikiCell(raw string) rawCell {
	// Find the first | not inside {{ }} or {| |}
	depth, tableDepth, splitIdx := 0, 0, -1
	for i := 0; i < len(raw); i++ {
		if i+1 < len(raw) && raw[i] == '{' && raw[i+1] == '{' {
			depth++
			i++
		} else if i+1 < len(raw) && raw[i] == '}' && raw[i+1] == '}' {
			if depth > 0 {
				depth--
			}
			i++
		} else if i+1 < len(raw) && raw[i] == '{' && raw[i+1] == '|' {
			tableDepth++
			i++
		} else if i+1 < len(raw) && raw[i] == '|' && raw[i+1] == '}' {
			if tableDepth > 0 {
				tableDepth--
			}
			i++
		} else if raw[i] == '|' && depth == 0 && tableDepth == 0 {
			splitIdx = i
			break
		}
	}

	if splitIdx < 0 || strings.TrimSpace(raw[:splitIdx]) == "" {
		return rawCell{content: strings.TrimSpace(raw), colspan: 1, rowspan: 1}
	}

	attr := raw[:splitIdx]
	return rawCell{
		content: strings.TrimSpace(raw[splitIdx+1:]),
		colspan: parseSpan(reColspan, attr),
		rowspan: parseSpan(reRowspan, attr),
	}
}

// splitTemplateParams splits template content on | while respecting nested {{ }} and [[ ]]
func splitTemplateParams(s string) []string {
	var params []string
	depth := 0
	start := 0

	for i := 0; i < len(s); i++ {
		switch {
		case i+1 < len(s) && s[i] == '{' && s[i+1] == '{':
			depth++
			i++
		case i+1 < len(s) && s[i] == '[' && s[i+1] == '[':
			depth++
			i++
		case i+1 < len(s) && s[i] == '}' && s[i+1] == '}':
			depth--
			i++
		case i+1 < len(s) && s[i] == ']' && s[i+1] == ']':
			depth--
			i++
		case s[i] == '|' && depth == 0:
			if p := paramValue(s[start:i]); p != "" {
				params = append(params, p)
			}
			start = i + 1
		}
	}
	if p := paramValue(s[start:]); p != "" {
		params = append(params, p)
	}
	return params
}

func paramValue(s string) string {
	s = strings.TrimSpace(s)
	if eq := strings.IndexByte(s, '='); eq >= 0 {
		s = strings.TrimSpace(s[eq+1:])
	}
	return s
}
