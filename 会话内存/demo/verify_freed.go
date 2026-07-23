package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type FreedAlive struct{ Objs, Bytes, Sites int }
type SiteLine struct{ Func, Loc string; Objs, Bytes int }
type GCBlock struct {
	GC                                int
	Freed                             *FreedAlive
	Alive                             *FreedAlive
	FreedSites, AliveSites            []SiteLine
}

var (
	freedSumRe = regexp.MustCompile(`gcdeadsession:freed:\s*(\d+) session objs \((\d+) bytes\) freed from (\d+) sites`)
	aliveSumRe = regexp.MustCompile(`gcdeadsession:alive:\s*(\d+) session objs \((\d+) bytes\) still alive from (\d+) sites`)
	gcHeaderRe = regexp.MustCompile(`=== GC #(\d+) ===`)
	siteLineRe = regexp.MustCompile(`\s+(.+) \((\S+:\d+)\).*:\s*(\d+) session objs, (\d+) session bytes\s+`)
)

func parseInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func verifyFile(path string) {
	f, _ := os.Open(path)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	var gcs []GCBlock
	i := 0
	for i < len(lines) && !gcHeaderRe.MatchString(lines[i]) {
		i++
	}
	for i < len(lines) {
		m := gcHeaderRe.FindStringSubmatch(lines[i])
		if m == nil { i++; continue }
		gcNum := parseInt(m[1])
		i++
		block := GCBlock{GC: gcNum}
		for i < len(lines) && !gcHeaderRe.MatchString(lines[i]) {
			line := lines[i]
			if fm := freedSumRe.FindStringSubmatch(line); fm != nil {
				block.Freed = &FreedAlive{Objs: parseInt(fm[1]), Bytes: parseInt(fm[2]), Sites: parseInt(fm[3])}
				i++
				for i < len(lines) {
					sl := siteLineRe.FindStringSubmatch(lines[i])
					if sl == nil { break }
					block.FreedSites = append(block.FreedSites, SiteLine{sl[1], sl[2], parseInt(sl[3]), parseInt(sl[4])})
					i++
				}
				continue
			}
			if am := aliveSumRe.FindStringSubmatch(line); am != nil {
				block.Alive = &FreedAlive{Objs: parseInt(am[1]), Bytes: parseInt(am[2]), Sites: parseInt(am[3])}
				i++
				for i < len(lines) {
					sl := siteLineRe.FindStringSubmatch(lines[i])
					if sl == nil { break }
					block.AliveSites = append(block.AliveSites, SiteLine{sl[1], sl[2], parseInt(sl[3]), parseInt(sl[4])})
					i++
				}
				continue
			}
			i++
		}
		gcs = append(gcs, block)
	}

	// Per-GC freed comparison
	freedMismatch := 0
	aliveMismatch := 0
	for _, gc := range gcs {
		gcSiteFreedBytes := 0
		for _, s := range gc.FreedSites { gcSiteFreedBytes += s.Bytes }
		gcSummaryFreedBytes := 0
		if gc.Freed != nil { gcSummaryFreedBytes = gc.Freed.Bytes }
		if gcSiteFreedBytes != gcSummaryFreedBytes {
			freedMismatch++
		}

		gcSiteAliveBytes := 0
		for _, s := range gc.AliveSites { gcSiteAliveBytes += s.Bytes }
		gcSummaryAliveBytes := 0
		if gc.Alive != nil { gcSummaryAliveBytes = gc.Alive.Bytes }
		if gcSiteAliveBytes != gcSummaryAliveBytes {
			aliveMismatch++
		}
	}

	// Total comparison
	totalSummaryFreedBytes := 0
	totalSiteFreedBytes := 0
	totalSummaryAliveBytes := 0
	totalSiteAliveBytes := 0
	for _, gc := range gcs {
		if gc.Freed != nil { totalSummaryFreedBytes += gc.Freed.Bytes }
		for _, s := range gc.FreedSites { totalSiteFreedBytes += s.Bytes }
		if gc.Alive != nil { totalSummaryAliveBytes += gc.Alive.Bytes }
		for _, s := range gc.AliveSites { totalSiteAliveBytes += s.Bytes }
	}

	base := strings.TrimSuffix(strings.TrimSuffix(path, ".log"), ".txt")
	base = strings.TrimPrefix(base, "output/")

	fmt.Printf("%-40s Freed: %s (summary=%d sites=%d, %d/%d GC mismatch)  Alive: %s (summary=%d sites=%d, %d/%d GC mismatch)\n",
		base,
		yesno(totalSummaryFreedBytes == totalSiteFreedBytes), totalSummaryFreedBytes, totalSiteFreedBytes, freedMismatch, len(gcs),
		yesno(totalSummaryAliveBytes == totalSiteAliveBytes), totalSummaryAliveBytes, totalSiteAliveBytes, aliveMismatch, len(gcs))
}

func yesno(v bool) string {
	if v { return "OK" }
	return "MISMATCH"
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		for _, dir := range []string{".", "output"} {
			for _, ext := range []string{"gcdeadtrace_*.txt", "gcdeadtrace_*.log"} {
				matches, _ := filepath.Glob(filepath.Join(dir, ext))
				args = append(args, matches...)
			}
		}
	}
	for _, path := range args {
		verifyFile(path)
	}
}
