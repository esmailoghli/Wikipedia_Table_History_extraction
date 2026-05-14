# Wikitables

Extracts and tracks tables across all revisions of every page in a MediaWiki XML dump (e.g. English Wikipedia). Output is one JSONL file per dump file, with one JSON object per (page, logical table) pair containing all revisions of that table.

## Build

```
go build .
```

## Usage

```
# Single file
wikitables file.xml.bz2

# All *.xml.bz2 in the current directory
wikitables --all

# From a list of paths
wikitables --input-list files.txt
```

## Flags

| Flag | Description |
|---|---|
| `--jobs N` | Process N files in parallel (default: 1) |
| `--done-list FILE` | Track completed files for safe resume after interruption |
| `--exclude-page ID` | Skip a page by ID; may be repeated |
| `--tmp-dir DIR` | Directory for intermediate spool files (default: OS temp) |

## Output format

Each line is a JSON object:

```json
{
  "pageID": 123,
  "pageTitle": "Example",
  "tableID": "456-0",
  "deletedAt": null,
  "tables": [
    {
      "revisionID": 789,
      "revisionDate": "Mon Jan 02 15:04:05 CET 2006",
      "contributor": {"username": "Alice", "ip": null, "id": "42"},
      "format": "wikitable",
      "artificialColumnHeaders": ["uuid-1", "uuid-2"],
      "matchScore": 0.95,
      "cells": [[{"content": "A", "wasExpanded": false}, ...], ...]
    }
  ]
}
```

`matchScore` is absent for the first revision of a table. `deletedAt` is present when the table was removed before the final revision of the page.