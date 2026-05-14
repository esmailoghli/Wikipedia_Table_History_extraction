package main

import "time"

// intermediate representation produced before span expansion
type rawCell struct {
	content          string
	colspan, rowspan int // default to 1 when absent from the markup
}

// Cell is one entry in the final expanded grid written to JSON output.
// WasExpanded is true for every cell that was synthetically inserted to fill
// a colspan or rowspan — i.e. it is a copy of an adjacent original cell.
type Cell struct {
	Content     string `json:"content"`
	WasExpanded bool   `json:"wasExpanded"`
}

// Contributor holds the editor identity for one revision. All fields are
// pointers so that absent values serialise as JSON null rather than "".
// Registered editors have Username and ID; anonymous editors have IP only.
type Contributor struct {
	Username *string `json:"username"`
	IP       *string `json:"ip"`
	ID       *string `json:"id"`
}

// revisionHeader is RevisionEntry without the Cells field.
// Used so we can json.Marshal the fixed fields and then stream cells separately.
// MatchScore is nil for the first revision of a table (no prior match exists);
// for subsequent revisions it holds the similarity score [0,1] from matchTables.
type revisionHeader struct {
	RevisionID              any         `json:"revisionID"`
	RevisionDate            string      `json:"revisionDate"`
	Contributor             Contributor `json:"contributor"`
	ArtificialColumnHeaders []string    `json:"artificialColumnHeaders"`
	Format                  string      `json:"format"`
	MatchScore              *float64    `json:"matchScore,omitempty"`
}

// deletionInfo is written to the top-level "deletedAt" field when a tracked
// table was absent from the page's final revision. RevisionID is the first
// revision in which the table was found to be missing.
type deletionInfo struct {
	RevisionID   any    `json:"revisionID"`
	RevisionDate string `json:"revisionDate"`
}

// used to display timestamps in Central European Time
var berlinLoc *time.Location

func init() {
	var err error
	berlinLoc, err = time.LoadLocation("Europe/Berlin")
	if err != nil {
		berlinLoc = time.UTC
	}
}

func prettifyTimestamp(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.In(berlinLoc).Format("Mon Jan 02 15:04:05 MST 2006")
}
