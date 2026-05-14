package main

import (
	"crypto/md5"
	"regexp"
	"sort"
	"strings"
)

var reHTMLTag = regexp.MustCompile(`<[^>]+>`)
var reWhitespace = regexp.MustCompile(`\s+`)

// fingerprintRaw hashes unexpanded raw rows for cheap revision deduplication
func fingerprintRaw(rawRows [][]rawCell) [16]byte {
	h := md5.New()
	for _, row := range rawRows {
		for _, c := range row {
			h.Write([]byte(strings.TrimSpace(c.content)))
			h.Write([]byte{byte(c.colspan), byte(c.rowspan)})
		}
		h.Write([]byte{0xff}) //0xff between rows to distinguish cell and row boundary shift
	}
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

func maxColsRaw(rawRows [][]rawCell) int {
	maxCols := 0
	for _, row := range rawRows {
		rowWidth := 0
		for _, cell := range row {
			rowWidth += cell.colspan
		}
		if rowWidth > maxCols {
			maxCols = rowWidth
		}
	}
	return maxCols
}

type tableSig struct { // lightweight summary of a table used for fuzzy matching across revisions
	maxCols int                 // width of the widest row
	words   map[string]struct{} // set of distinct lowercase words found in the first three rows
}

// tableSignature builds a tableSig from the first three rows of cells
func tableSignature(cells [][]Cell) tableSig {
	if len(cells) == 0 {
		return tableSig{}
	}
	maxCols := 0
	for _, row := range cells {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}
	words := map[string]struct{}{}
	limit := 3
	if len(cells) < limit {
		limit = len(cells)
	}
	for _, row := range cells[:limit] {
		for _, c := range row {
			stripped := reHTMLTag.ReplaceAllString(c.Content, "") //Strip HTML tags
			stripped = strings.ToLower(strings.TrimSpace(reWhitespace.ReplaceAllString(stripped, " ")))
			for _, w := range strings.Fields(stripped) {
				words[w] = struct{}{}
			}
		}
	}
	return tableSig{maxCols: maxCols, words: words}
}

// sigSimilarity returns a score in [0, 1] measuring how likely two signatures describe the same table.
// Uses overlap/min(|words_a|, |words_b|)
func sigSimilarity(a, b tableSig) float64 {
	if len(a.words) == 0 && len(b.words) == 0 {
		if a.maxCols == b.maxCols {
			return 1.0
		}
		return 0.0
	}

	if len(a.words) == 0 || len(b.words) == 0 {
		return 0.0
	}

	intersection := 0
	for word := range a.words {
		if _, ok := b.words[word]; ok {
			intersection++
		}
	}

	smaller := len(a.words)
	if len(b.words) < smaller {
		smaller = len(b.words)
	}

	similarity := 0.0
	if smaller > 0 {
		similarity = float64(intersection) / float64(smaller)
	}
	if a.maxCols != b.maxCols { //30 % penalty when the column counts differ
		similarity *= 0.7
	}
	return similarity
}

// candidate is one (tracked table, current table) pair with its similarity score
type candidate struct {
	score  float64
	ti, ci int // tracked-table index, current-table index
}

// matchResult holds the outcome for one current table
type matchResult struct {
	ti    int     // index of the matched tracked table (or -1 for a new table)
	score float64 //similarity score, 0 when unmatched
}

// matchTables performs greedy bipartite matching between the current tables and tracked tables
func matchTables(tracked []*tableState, currentSigs []tableSig) []matchResult {
	result := make([]matchResult, len(currentSigs))
	for i := range result {
		result[i] = matchResult{ti: -1}
	}
	if len(tracked) == 0 {
		return result
	}

	var candidates []candidate
	for ci, currentSig := range currentSigs {
		for ti, tracked := range tracked {
			s := sigSimilarity(currentSig, tracked.sig)
			if s >= 0.3 { // sufficiently similar match (score ≥ 0.3) needed
				candidates = append(candidates, candidate{s, ti, ci})
			}
		}
	}
	// Sort descending by score so the strongest matches are consumed first.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	usedT := map[int]bool{}
	usedC := map[int]bool{}
	for _, c := range candidates {
		if usedT[c.ti] || usedC[c.ci] {
			continue
		}
		result[c.ci] = matchResult{ti: c.ti, score: c.score}
		usedT[c.ti] = true
		usedC[c.ci] = true
	}
	return result
}
