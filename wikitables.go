package main

import (
	"bufio"
	"compress/bzip2"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type pageIDSet map[string]bool

func (s *pageIDSet) String() string { return "" }
func (s *pageIDSet) Set(v string) error {
	if *s == nil {
		*s = make(pageIDSet)
	}
	(*s)[strings.TrimSpace(v)] = true
	return nil
}

// run processes one MediaWiki XML dump file and writes a JSONL file to the current directory
func run(inputPath string, showProgress bool, tmpDir string, excludePages pageIDSet) error {
	base := filepath.Base(inputPath)
	base = strings.TrimSuffix(base, ".bz2")
	base = strings.TrimSuffix(base, ".xml")
	outputPath := base + ".jsonl"

	if showProgress {
		fmt.Fprintf(os.Stderr, "Input:  %s\nOutput: %s\n", inputPath, outputPath)
	}

	f, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var xmlReader io.Reader
	if strings.HasSuffix(inputPath, ".bz2") {
		xmlReader = bzip2.NewReader(f)
	} else {
		xmlReader = f
	}

	if _, err := os.Stat(outputPath); err == nil {
		return fmt.Errorf("skip: output file already exists: %s", outputPath)
	}

	outF, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outF.Close()
	outW := bufio.NewWriterSize(outF, 1<<20)
	defer outW.Flush()

	decoder := xml.NewDecoder(xmlReader)

	var (
		inPage        bool
		inRevision    bool
		inContributor bool
		// pageIDDone prevents the first <id> inside a <revision> from being
		// mistaken for the page ID; the page ID is the very first <id> child
		// of <page>, before any <revision>.
		pageIDDone bool

		ps = pageState{tmpDir: tmpDir}

		// Fields accumulated for the revision currently being parsed.
		revID   string
		revTS   string
		revUser string
		revIP   string
		revCID  string // contributor ID
		revText strings.Builder

		// capturing / capBuf implement a simple "collect all CharData into a
		// buffer" pattern. On each StartElement that we care about, capturing
		// is set to that element's name. On the matching EndElement the buffer
		// is consumed and capturing is cleared. revText is handled separately
		// (at </revision>) because it can be very large.
		capturing string
		capBuf    strings.Builder

		skipPage bool // true when the current page's ID is in excludePages
		pages    int
		revsDone atomic.Int64
		t0       = time.Now()
	)

	// Progress bar — only in single-file mode; in --all mode the overall ticker handles it.
	stopProgress := make(chan struct{})
	if showProgress {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			lastPages := 0
			lastTick := t0
			for {
				select {
				case <-stopProgress:
					return
				case now := <-ticker.C:
					elapsed := now.Sub(t0).Seconds()
					dt := now.Sub(lastTick).Seconds()
					rate := 0.0
					if dt > 0 {
						rate = float64(pages-lastPages) / dt
					}
					lastPages = pages
					lastTick = now
					rss := rssKB() / 1024
					fmt.Fprintf(os.Stderr, "\r\033[K[%5.0fs] %7d pages  %6.0f p/s  %8d revs  %5d MB RSS",
						elapsed, pages, rate, revsDone.Load(), rss)
				}
			}
		}()
	}

	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("xml: %w", err)
		}

		switch t := tok.(type) {

		case xml.StartElement:
			name := t.Name.Local
			switch name {
			case "page":
				inPage = true
				pageIDDone = false
				skipPage = false
				ps.reset()

			case "revision":
				if inPage && !skipPage {
					inRevision = true
					revID, revTS, revUser, revIP, revCID = "", "", "", "", ""
					revText = strings.Builder{}
				}

			case "contributor":
				if inRevision {
					inContributor = true
				}

			case "title":
				if inPage {
					capturing = "title"
					capBuf.Reset()
				}
			case "id":
				if inPage {
					capturing = "id"
					capBuf.Reset()
				}
			case "timestamp":
				if inRevision {
					capturing = "timestamp"
					capBuf.Reset()
				}
			case "username":
				if inContributor {
					capturing = "username"
					capBuf.Reset()
				}
			case "ip":
				if inContributor {
					capturing = "ip"
					capBuf.Reset()
				}
			case "text":
				if inRevision {
					capturing = "text"
					revText.Reset()
				}
			}

		case xml.EndElement:
			name := t.Name.Local

			// When the closing tag matches what we were capturing, consume the buffer
			if capturing != "" && name == capturing {
				if capturing == "text" {
					// handled at </revision>
				} else {
					val := strings.Clone(strings.TrimSpace(capBuf.String()))
					switch capturing {
					case "title":
						ps.title = val
					case "id":
						// <id> appears in three contexts; priority order:
						//   1. inside <contributor>  → contributor ID
						//   2. inside <revision>     → revision ID
						//   3. direct child of <page> (first occurrence only) -> page ID
						if inContributor {
							revCID = val
						} else if inRevision {
							revID = val
						} else if inPage && !pageIDDone {
							ps.id = val
							ps.idAny = intOrStr(val)
							pageIDDone = true
							skipPage = excludePages[val]
						}
					case "timestamp":
						revTS = val
					case "username":
						revUser = val
					case "ip":
						revIP = val
					}
				}
				capturing = ""
			}

			switch name {
			case "contributor":
				inContributor = false

			case "revision":
				if inPage {
					inRevision = false
					contrib := Contributor{
						Username: strPtr(revUser),
						IP:       strPtr(revIP),
						ID:       strPtr(revCID),
					}
					revDate := prettifyTimestamp(revTS)
					before := tablesWithData(&ps)
					processRevision(&ps, revID, revDate, contrib, revText.String())
					// Count revisions that produced at least one new table entry.
					if tablesWithData(&ps) > before {
						revsDone.Add(1)
					}
					revText.Reset()
				}

			case "page":
				inPage = false
				if !skipPage {
					if err := flushPage(&ps, outW); err != nil {
						fmt.Fprintf(os.Stderr, "flush error on %q: %v\n", ps.title, err)
					}
					ps.reset()
					pages++
				}
			}

		case xml.CharData:
			switch capturing {
			case "text":
				revText.Write(t)
			case "":
				// not capturing anything
			default:
				capBuf.Write(t)
			}
		}
	}

	close(stopProgress)
	outW.Flush()
	if showProgress {
		fmt.Fprintf(os.Stderr, "\r\033[K")
		fmt.Fprintf(os.Stderr, "Done. %d pages  %d revs  %.1fs → %s\n",
			pages, revsDone.Load(), time.Since(t0).Seconds(), outputPath)
	}
	return nil
}

func loadDoneList(path string) map[string]bool {
	done := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return done // file doesn't exist yet, that's fine
	}
	for _, line := range strings.Split(string(data), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			done[s] = true
		}
	}
	return done
}

func appendDoneList(path, name string, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, name)
}

// runFile calls run() and handles the three possible outcomes:
//   - success: appends the filename to the done list
//   - "skip:" error: the output already existed; log and return without touching it
//   - real error: delete the partial output, write a .err sidecar file
func runFile(inputPath, doneList string, doneMu *sync.Mutex, tmpDir string, excludePages pageIDSet) {
	base := filepath.Base(inputPath)
	err := run(inputPath, false, tmpDir, excludePages)

	if err != nil {
		outBase := strings.TrimSuffix(strings.TrimSuffix(base, ".bz2"), ".xml")
		outPath := outBase + ".jsonl"

		// "skip:" prefix means the output already existed — don't touch it
		if strings.HasPrefix(err.Error(), "skip:") {
			fmt.Fprintf(os.Stderr, "SKIP %s: %v\n", base, err)
			return
		}

		// Real processing error — delete partial output, write .err sidecar file
		os.Remove(outPath)
		logPath := outBase + ".err"
		os.WriteFile(logPath, []byte(fmt.Sprintf("error processing %s:\n%v\n", inputPath, err)), 0644)
		fmt.Fprintf(os.Stderr, "ERROR %s: %v (see %s)\n", base, err, logPath)
		return
	}

	if doneList != "" {
		appendDoneList(doneList, base, doneMu)
	}
}

func main() {
	allFlag := flag.Bool("all", false,
		"process all *.xml.bz2 files found in the current directory")
	inputListFlag := flag.String("input-list", "", ""+
		"process files listed in `FILE` (one path per line)")
	jobsFlag := flag.Int("jobs", 1,
		"number of files to process in parallel (meaningful with --all or --input-list)")
	doneListFlag := flag.String("done-list", "", ""+
		"path to a done-list `FILE`; when a file finishes successfully its base\n"+
		"name is appended to FILE, and files already listed are skipped on the\n"+
		"next run — enables safe resume after an interrupted batch")
	tmpDirFlag := flag.String("tmp-dir", "", ""+
		"directory for intermediate per-table spool `files`;\n"+
		"defaults to the OS temp directory")

	var excludePages pageIDSet
	flag.Var(&excludePages, "exclude-page",
		"skip pages whose ID matches `pageID`; may be repeated")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  wikitables file.xml[.bz2]                          process a single dump file\n")
		fmt.Fprintf(os.Stderr, "  wikitables --all                                   process all *.xml.bz2 in the current directory\n")
		fmt.Fprintf(os.Stderr, "  wikitables --input-list FILE                       process files listed in FILE\n")
		fmt.Fprintf(os.Stderr, "  wikitables --exclude-page 123 --exclude-page 456   skip specific pages\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Output is written to <basename>.jsonl in the current directory.\n")
		fmt.Fprintf(os.Stderr, "Each output line is a JSON object with the fields:\n")
		fmt.Fprintf(os.Stderr, "  pageID, pageTitle, tableID, tables[]\n")
		fmt.Fprintf(os.Stderr, "where each tables entry has:\n")
		fmt.Fprintf(os.Stderr, "  revisionID, revisionDate, contributor, artificialColumnHeaders, cells[][]\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.VisitAll(func(f *flag.Flag) {
			name, usage := flag.UnquoteUsage(f)
			if name != "" {
				fmt.Fprintf(os.Stderr, "  --%s %s\n    \t%s\n", f.Name, name,
					strings.ReplaceAll(usage, "\n", "\n    \t"))
			} else {
				fmt.Fprintf(os.Stderr, "  --%s\n    \t%s\n", f.Name,
					strings.ReplaceAll(usage, "\n", "\n    \t"))
			}
		})
	}

	flag.Parse()

	modes := 0
	if *allFlag {
		modes++
	}
	if *inputListFlag != "" {
		modes++
	}
	if flag.NArg() > 0 {
		modes++
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr, "Error: specify at most one of --all, --input-list, or a file argument.")
		flag.Usage()
		os.Exit(1)
	}
	if modes == 0 {
		flag.Usage()
		os.Exit(1)
	}

	// single file
	if flag.NArg() > 0 {

		if err := run(flag.Arg(0), true, *tmpDirFlag, excludePages); err != nil {
			if strings.HasPrefix(err.Error(), "skip:") {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(0)
			}
			base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(flag.Arg(0)), ".bz2"), ".xml")
			os.Remove(base + ".jsonl")
			os.WriteFile(base+".err", []byte(fmt.Sprintf("error processing %s:\n%v\n", flag.Arg(0), err)), 0644)
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if *doneListFlag != "" {
			var mu sync.Mutex
			appendDoneList(*doneListFlag, filepath.Base(flag.Arg(0)), &mu)
		}
		return
	}

	// --all / --input-list mode
	var entries []string
	if *inputListFlag != "" {
		data, err := os.ReadFile(*inputListFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading input list: %v\n", err)
			os.Exit(1)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if p := strings.TrimSpace(line); p != "" {
				entries = append(entries, p)
			}
		}
		if len(entries) == 0 {
			fmt.Fprintln(os.Stderr, "No files found in input list.")
			os.Exit(1)
		}
	} else {
		var err error
		entries, err = filepath.Glob("*.bz2")
		if err != nil || len(entries) == 0 {
			fmt.Fprintln(os.Stderr, "No .bz2 files found in current directory.")
			os.Exit(1)
		}
	}

	done := map[string]bool{}
	if *doneListFlag != "" {
		done = loadDoneList(*doneListFlag)
	}

	// Delete any .jsonl file that belongs to our input set but is absent from the done list
	if *doneListFlag != "" {
		jsonls, _ := filepath.Glob("*.jsonl")
		// Restrict cleanup to files we are responsible
		ours := map[string]bool{}
		for _, e := range entries {
			base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(e), ".bz2"), ".xml")
			ours[base+".jsonl"] = true
		}
		removed := 0
		for _, jf := range jsonls {
			if !ours[filepath.Base(jf)] {
				continue // not our responsibility — leave it alone
			}
			base := strings.TrimSuffix(filepath.Base(jf), ".jsonl")
			if !done[base+".bz2"] {
				if err := os.Remove(jf); err == nil {
					fmt.Fprintf(os.Stderr, "Removed incomplete output: %s\n", jf)
					removed++
				}
			}
		}
		if removed > 0 {
			fmt.Fprintf(os.Stderr, "Removed %d incomplete file(s).\n", removed)
		}
	}

	var files []string
	for _, f := range entries {
		if !done[filepath.Base(f)] {
			files = append(files, f)
		}
	}

	skipped := len(entries) - len(files)
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "Skipping %d already-completed file(s).\n", skipped)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "Nothing left to process.")
		return
	}

	fmt.Fprintf(os.Stderr, "Processing %d file(s) with %d worker(s)...\n", len(files), *jobsFlag)

	var (
		doneMu    sync.Mutex
		wg        sync.WaitGroup
		sem       = make(chan struct{}, *jobsFlag)
		t0        = time.Now()
		completed atomic.Int64
	)

	// Overall progress ticker
	stopTicker := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTicker:
				return
			case <-ticker.C:
				rss := rssKB() / 1024
				fmt.Fprintf(os.Stderr, "\r\033[K[%5.0fs] %d/%d files done  %d MB RSS",
					time.Since(t0).Seconds(), completed.Load(), int64(len(files)), rss)
			}
		}
	}()

	for _, f := range files {
		wg.Add(1)
		sem <- struct{}{} // acquire slot
		go func(path string) {
			defer wg.Done()
			defer func() { <-sem }()
			runFile(path, *doneListFlag, &doneMu, *tmpDirFlag, excludePages)
			completed.Add(1)
		}(f)
	}

	wg.Wait()
	close(stopTicker)
	fmt.Fprintf(os.Stderr, "\r\033[K")
	fmt.Fprintf(os.Stderr, "All done. %d/%d files completed in %.1fs.\n",
		completed.Load(), int64(len(files)), time.Since(t0).Seconds())
}
