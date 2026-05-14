package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
)

// ensureUUIDs grows uuids to at least `needed` entries.
func ensureUUIDs(uuids []string, needed int) []string {
	for len(uuids) < needed {
		uuids = append(uuids, uuid.NewString())
	}
	return uuids
}

func intOrStr(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return s
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	cp := strings.Clone(s)
	return &cp
}

type tableInfo struct {
	origIdx     int
	signature   tableSig
	fingerprint [16]byte
	maxCols     int
	empty       bool
}

// computeTableInfo parses one table string and extracts sig/fp/maxCols
func computeTableInfo(origIdx int, ttext string) tableInfo {
	rawRows := tableToRawRows(ttext)
	if len(rawRows) == 0 {
		return tableInfo{empty: true}
	}

	// Build a minimal [][]Cell from raw rows for tableSignature
	limit := 3
	if len(rawRows) < limit {
		limit = len(rawRows)
	}

	sigCells := make([][]Cell, limit)
	for i, row := range rawRows[:limit] {
		sigCells[i] = make([]Cell, len(row))
		for j, c := range row {
			sigCells[i][j] = Cell{Content: c.content}
		}
	}
	return tableInfo{
		origIdx:     origIdx,
		signature:   tableSignature(sigCells),
		fingerprint: fingerprintRaw(rawRows),
		maxCols:     maxColsRaw(rawRows),
	}
}

// tableState tracks one logical table across all revisions of a page
type tableState struct {
	tableID  string
	sig      tableSig // updated each revision to reflect the latest structure
	colUUIDs []string // assigns a stable UUID to each logical column
	lastFP   [16]byte // fingerprint of the last written revision; used to skip duplicates
	tmpFile  *os.File // revision entries written here as JSONL, one per line
	tmpPath  string

	//both get cleared if table reappears later
	deletedInRevID   any
	deletedInRevDate string
}

func (st *tableState) close() {
	if st.tmpFile != nil {
		st.tmpFile.Close()
		st.tmpFile = nil
	}
}

func (st *tableState) remove() {
	if st.tmpPath != "" {
		os.Remove(st.tmpPath)
		st.tmpPath = ""
	}
}

type pageState struct { // mutable state accumulated while processing one <page>
	id     string // raw string form of the page ID (may be non-numeric)
	idAny  any    // int or string depending on intOrStr result; used in JSON output
	title  string
	tables []*tableState
	tmpDir string // directory for temp spool files; "" means the OS default
}

// reset clears all fields and closes/removes any open spool files so the
// struct can be reused for the next page without a heap allocation.
func (ps *pageState) reset() {
	ps.id = ""
	ps.idAny = nil
	ps.title = ""
	for i, st := range ps.tables {
		st.close()
		st.remove()
		ps.tables[i] = nil
	}
	ps.tables = ps.tables[:0]
}

func tableFormat(raw string) string {
	if strings.HasPrefix(strings.TrimSpace(raw), "{|") {
		return "wikitable"
	}
	return "html"
}

func updateDeletionMarkers(ps *pageState, nTracked int, matchedTracked map[int]bool, revID, revDate string) {
	var zero [16]byte
	for ti := 0; ti < nTracked; ti++ {
		state := ps.tables[ti]
		if matchedTracked[ti] {
			// Table is present in this revision — clear any pending deletion marker.
			state.deletedInRevID = nil
			state.deletedInRevDate = ""
		} else if state.deletedInRevID == nil && state.lastFP != zero {
			// Table was seen before but absent in this revision — record the
			// first revision in which it went missing.
			state.deletedInRevID = intOrStr(revID)
			state.deletedInRevDate = revDate
		}
	}
}

func processRevision(ps *pageState, revID, revDate string, contrib Contributor, text string) {
	// nTracked captures how many tables were tracked before this revision so
	// that updateDeletionMarkers only considers pre-existing tables and never
	// mistakes a newly created table for a deleted one.
	nTracked := len(ps.tables)

	rawTables := extractTables(text)
	if len(rawTables) == 0 {
		updateDeletionMarkers(ps, nTracked, nil, revID, revDate)
		return
	}

	// Pass 1: compute sig/fp/maxCols for each table
	infos := make([]tableInfo, len(rawTables))
	anyValid := false
	for i, ttext := range rawTables {
		infos[i] = computeTableInfo(i, ttext)
		if !infos[i].empty {
			anyValid = true
		}
	}
	if !anyValid {
		updateDeletionMarkers(ps, nTracked, nil, revID, revDate)
		return
	}

	// Build sigs slice for matching
	sigs := make([]tableSig, 0, len(rawTables))
	validIdx := make([]int, 0, len(rawTables)) // maps sigs index to rawTables index
	for i, info := range infos {
		if !info.empty {
			sigs = append(sigs, info.signature)
			validIdx = append(validIdx, i)
		}
	}
	matches := matchTables(ps.tables, sigs)

	revIDany := intOrStr(revID)

	// Pass 2: reparse each valid table, marshal immediately, free cells
	for si, rawIdx := range validIdx {
		info := infos[rawIdx]
		m := matches[si]

		var state *tableState
		var matchScore *float64
		if m.ti >= 0 {
			state = ps.tables[m.ti]
			state.sig = info.signature
			if info.fingerprint == state.lastFP {
				continue
			}
			s := m.score
			matchScore = &s
		} else {
			f, err := os.CreateTemp(ps.tmpDir, fmt.Sprintf("wt-%v-*.jsonl", ps.id))
			if err != nil {
				fmt.Fprintf(os.Stderr, "temp file error: %v\n", err)
				continue
			}
			state = &tableState{
				tableID: fmt.Sprintf("%s-%d", revID, info.origIdx),
				sig:     info.signature,
				tmpFile: f,
				tmpPath: f.Name(),
			}
			ps.tables = append(ps.tables, state)
		}

		state.lastFP = info.fingerprint
		state.colUUIDs = ensureUUIDs(state.colUUIDs, info.maxCols)

		hdrBytes, hdrErr := json.Marshal(revisionHeader{
			RevisionID:              revIDany,
			RevisionDate:            revDate,
			Contributor:             contrib,
			ArtificialColumnHeaders: append([]string(nil), state.colUUIDs[:info.maxCols]...),
			Format:                  tableFormat(rawTables[rawIdx]),
			MatchScore:              matchScore,
		})

		if hdrErr != nil {
			fmt.Fprintf(os.Stderr, "json marshal error: %v\n", hdrErr)
			continue
		}

		// json.Marshal produces a complete JSON object ending with '}'.
		// Strip that closing brace so we can inject the "cells" field and
		// then close the object ourselves, forming one complete JSONL record.
		state.tmpFile.Write(hdrBytes[:len(hdrBytes)-1])
		state.tmpFile.WriteString(`,"cells":`)
		if cellErr := tableCellsToJSON(rawTables[rawIdx], state.tmpFile); cellErr != nil {
			fmt.Fprintf(os.Stderr, "json cells error: %v\n", cellErr)
			continue
		}
		state.tmpFile.WriteString("}\n")
	}

	// Build the set of pre-existing tracked tables that were matched in this
	// revision, then update deletion markers for all pre-existing tables.
	matchedTracked := make(map[int]bool, len(matches))
	for _, m := range matches {
		if m.ti >= 0 {
			matchedTracked[m.ti] = true
		}
	}
	updateDeletionMarkers(ps, nTracked, matchedTracked, revID, revDate)
}

// flushPage reads each table's spool file and writes one JSONL record per tracked table to out
func flushPage(ps *pageState, out *bufio.Writer) error {
	for _, tableState := range ps.tables {
		if tableState.tmpFile == nil {
			continue
		}
		tableState.tmpFile.Sync()
		tableState.tmpFile.Close()
		tableState.tmpFile = nil

		f, err := os.Open(tableState.tmpPath)
		if err != nil {
			os.Remove(tableState.tmpPath)
			continue
		}

		var deletedAt *deletionInfo
		if tableState.deletedInRevID != nil {
			deletedAt = &deletionInfo{
				RevisionID:   tableState.deletedInRevID,
				RevisionDate: tableState.deletedInRevDate,
			}
		}

		prefix, err := json.Marshal(struct {
			PageID    any           `json:"pageID"`
			PageTitle string        `json:"pageTitle"`
			TableID   string        `json:"tableID"`
			DeletedAt *deletionInfo `json:"deletedAt,omitempty"`
		}{ps.idAny, ps.title, tableState.tableID, deletedAt})

		if err != nil {
			f.Close()
			os.Remove(tableState.tmpPath)
			continue
		}

		br := bufio.NewReaderSize(f, 4*1024*1024)
		first := true
		wroteAny := false
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 && line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				if first {
					out.Write(prefix[:len(prefix)-1]) // open the output object
					out.WriteString(`,"tables":[`)
					first = false
				} else {
					out.WriteByte(',')
				}
				out.Write(line)
				wroteAny = true
			}
			if err != nil {
				break
			}
		}
		if wroteAny {
			out.WriteString("]}\n")
		}
		f.Close()
		os.Remove(tableState.tmpPath)
		tableState.tmpPath = ""
	}
	return nil
}
