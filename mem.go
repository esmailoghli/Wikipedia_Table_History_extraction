package main

import (
	"fmt"
	"os"
	"strings"
)

// rssKB reads VmRSS from /proc/self/status (Linux only; returns 0 elsewhere).
func rssKB() int {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb int
			fmt.Sscanf(strings.TrimPrefix(line, "VmRSS:"), "%d", &kb)
			return kb
		}
	}
	return 0
}

// tablesWithData counts tracked tables that have had at least one revision written.
func tablesWithData(ps *pageState) int {
	n := 0
	for _, t := range ps.tables {
		if t.tmpPath != "" {
			n++
		}
	}
	return n
}
