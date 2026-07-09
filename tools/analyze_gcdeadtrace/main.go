// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Analyze parses a gcdeadtrace output file and finds allocation call sites
// where ALL session-allocated objects have died (100% dead rate).
//
// Usage: go run . -input=<input_file> -output=<output_file>
//
// The input file must contain gcdeadsession:freed: and gcdeadsession:alive:
// sections produced by GODEBUG=gcdeadtrace=1 with session tracking enabled.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	input := flag.String("input", "", "input gcdeadtrace output file")
	output := flag.String("output", "fully_dead_sites.txt", "output analysis file")
	flag.Parse()

	if *input == "" {
		fmt.Fprintln(os.Stderr, "usage: go run . -input=<input_file> [-output=<output_file>]")
		os.Exit(1)
	}

	data, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read input: %v\n", err)
		os.Exit(1)
	}

	result := analyze(string(data))

	if err := os.WriteFile(*output, []byte(result), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write output: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("analysis written to %s\n", *output)
}

// siteKey extracts the full call stack string from a site line.
// Example input: "  func1 (file:line) < func2 (file:line): N session objs, M session bytes"
// Returns the call stack portion: "func1 (file:line) < func2 (file:line)"
func siteKey(line string) string {
	line = strings.TrimSpace(line)
	idx := strings.LastIndex(line, ": ")
	if idx < 0 {
		return ""
	}
	after := strings.TrimSpace(line[idx+2:])
	if len(after) == 0 || (after[0] < '0' || after[0] > '9') {
		return ""
	}
	return strings.TrimSpace(line[:idx])
}

// parseSection extracts (siteKey, count, bytes) entries from a section.
func parseSection(lines []string, startMarker string) map[string][2]uintptr {
	result := make(map[string][2]uintptr)
	inSection := false

	for _, l := range lines {
		trimmed := strings.TrimSpace(l)

		if strings.HasPrefix(trimmed, startMarker) {
			inSection = true
			continue
		}

		if !inSection {
			continue
		}

		// End of section: next section header or empty line or OK
		if trimmed == "" || trimmed == "OK" ||
			(strings.HasPrefix(trimmed, "gcdead") && !strings.HasPrefix(trimmed, startMarker)) {
			inSection = false
			continue
		}

		if !strings.Contains(trimmed, "session objs") {
			continue
		}
		key := siteKey(l)
		if key == "" {
			continue
		}

		// Format: "...: N session objs, M session bytes"
		parts := strings.SplitN(trimmed, ": ", 2)
		if len(parts) < 2 {
			continue
		}
		countAndBytes := parts[len(parts)-1]
		// Extract both numbers from e.g. "5 session objs, 320 session bytes"
		var nums []uintptr
		for _, f := range strings.Fields(countAndBytes) {
			// Strip trailing comma: "objs," → "objs"
			f = strings.TrimRight(f, ",")
			var v uintptr
			if n, _ := fmt.Sscanf(f, "%d", &v); n == 1 {
				nums = append(nums, v)
			}
		}
		if len(nums) < 1 {
			continue
		}
		count := nums[0]
		bytes := uintptr(0)
		if len(nums) > 1 {
			bytes = nums[1]
		}

		existing := result[key]
		existing[0] += count
		existing[1] += bytes
		result[key] = existing
	}
	return result
}

func analyze(content string) string {
	lines := strings.Split(content, "\n")

	freedSites := parseSection(lines, "gcdeadsession:freed:")
	aliveSites := parseSection(lines, "gcdeadsession:alive:")

	type siteInfo struct {
		key   string
		count uintptr  // total objs at this site
		bytes uintptr
	}
	var fullyDead []siteInfo
	var partial []siteInfo
	var fullyAlive []siteInfo

	for key, fb := range freedSites {
		if ab, ok := aliveSites[key]; ok {
			partial = append(partial, siteInfo{key, fb[0] + ab[0], fb[1] + ab[1]})
		} else {
			fullyDead = append(fullyDead, siteInfo{key, fb[0], fb[1]})
		}
	}
	for key, ab := range aliveSites {
		if _, ok := freedSites[key]; !ok {
			fullyAlive = append(fullyAlive, siteInfo{key, ab[0], ab[1]})
		}
	}

	var sb strings.Builder

	sb.WriteString("=== gcdeadtrace session analysis: fully-dead call sites ===\n")
	sb.WriteString("(Allocation call sites where ALL session objects died.)\n\n")

	if len(fullyDead) == 0 {
		sb.WriteString("No fully-dead call sites found.\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Found %d fully-dead call site(s):\n\n", len(fullyDead)))
		for _, s := range fullyDead {
			sb.WriteString(fmt.Sprintf("  [FULLY DEAD] %s\n", s.key))
			sb.WriteString(fmt.Sprintf("    dead: %d objs, %d bytes (100%% dead)\n\n", s.count, s.bytes))
		}
	}

	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("Partially dead call sites: %d\n", len(partial)))
	for _, s := range partial {
		sb.WriteString(fmt.Sprintf("  [PARTIAL] %s (%d objs total)\n", s.key, s.count))
	}
	sb.WriteString(fmt.Sprintf("\nFully alive call sites (0%% dead): %d\n", len(fullyAlive)))
	for _, s := range fullyAlive {
		sb.WriteString(fmt.Sprintf("  [ALIVE] %s (%d objs alive)\n", s.key, s.count))
	}

	return sb.String()
}
