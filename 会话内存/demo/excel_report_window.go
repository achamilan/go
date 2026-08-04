// excel_report_window — gcdeadwindow XLSX report generator.
//
// Reads gcdeadwindow report logs ("=== GC #N gcdeadwindow gen=G elapsed=Ts ==="
// blocks with gcdeadwindow:alive/alloc/freed sections) and generates a
// multi-sheet Excel file with Overview, per-file detail, and per-gen site
// attribution.
//
// Usage:
//
//	go build -o excel_report_window.exe excel_report_window.go
//	excel_report_window.exe [file1.log file2.log ...]
//	(no args: scans . and output/ for gcdeadwindow_*.log / gcdeadwindow_*.txt)
//
// Output: gcdeadwindow_report.xlsx in the current directory.
//
// No external Go dependencies (uses only archive/zip; XLSX XML is built
// with string concatenation, same as excel_report.go).
package main

import (
	"archive/zip"
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ── Parsed data model ─────────────────────────────────────────────

type winSite struct {
	stack string // full stack text ("f (file:line) < g (file:line)")
	fn    string // first frame function name
	loc   string // first frame file:line
	objs  int64
	bytes int64
}

type winBlock struct {
	gcNum   int
	gen     int
	elapsed int

	aliveObjs, aliveBytes, aliveSites int64
	allocObjs, allocBytes, allocSites int64
	freedObjs, freedBytes, freedSites int64

	hasAlive, hasAlloc, hasFreed bool

	aliveLines []winSite
	allocLines []winSite
	freedLines []winSite
}

type parsedWindow struct {
	name   string
	blocks []*winBlock
}

// ── Parsing ───────────────────────────────────────────────────────

var (
	gcHeaderRe = regexp.MustCompile(`^=== GC #(\d+) gcdeadwindow gen=(\d+) elapsed=(\d+)s ===`)
	aliveSumRe = regexp.MustCompile(`^gcdeadwindow:alive: (\d+) objs \((\d+) bytes\) still alive from (\d+) sites`)
	allocSumRe = regexp.MustCompile(`^gcdeadwindow:alloc: (\d+) objs \((\d+) bytes\) allocated this cycle from (\d+) sites`)
	freedSumRe = regexp.MustCompile(`^gcdeadwindow:freed: (\d+) objs \((\d+) bytes\) freed this cycle from (\d+) sites`)
	siteLineRe = regexp.MustCompile(`^  (.+): (\d+) objs, (\d+) bytes$`)
	frameRe    = regexp.MustCompile(`^(\S+) \(([^)]+)\)`)
)

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// parseWinSite parses a site detail line:
// "  func (file:line) < func2 (file:line): N objs, M bytes"
func parseWinSite(line string) *winSite {
	m := siteLineRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	s := winSite{stack: m[1], objs: atoi64(m[2]), bytes: atoi64(m[3])}
	if fm := frameRe.FindStringSubmatch(m[1]); fm != nil {
		s.fn = fm[1]
		s.loc = fm[2]
	} else {
		s.fn = m[1]
	}
	return &s
}

func parseWindowFile(path string) (*parsedWindow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Mode name: strip directory, gcdeadwindow_ prefix and extension.
	base := filepath.Base(path)
	name := strings.TrimPrefix(base, "gcdeadwindow_")
	if ext := filepath.Ext(name); ext != "" {
		name = strings.TrimSuffix(name, ext)
	}

	pd := &parsedWindow{name: name}
	var blk *winBlock
	section := "" // "alive" | "alloc" | "freed"

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")

		if m := gcHeaderRe.FindStringSubmatch(line); m != nil {
			blk = &winBlock{
				gcNum:   int(atoi64(m[1])),
				gen:     int(atoi64(m[2])),
				elapsed: int(atoi64(m[3])),
			}
			pd.blocks = append(pd.blocks, blk)
			section = ""
			continue
		}
		if blk == nil {
			continue
		}
		if m := aliveSumRe.FindStringSubmatch(line); m != nil {
			blk.hasAlive = true
			blk.aliveObjs, blk.aliveBytes, blk.aliveSites = atoi64(m[1]), atoi64(m[2]), atoi64(m[3])
			section = "alive"
			continue
		}
		if m := allocSumRe.FindStringSubmatch(line); m != nil {
			blk.hasAlloc = true
			blk.allocObjs, blk.allocBytes, blk.allocSites = atoi64(m[1]), atoi64(m[2]), atoi64(m[3])
			section = "alloc"
			continue
		}
		if m := freedSumRe.FindStringSubmatch(line); m != nil {
			blk.hasFreed = true
			blk.freedObjs, blk.freedBytes, blk.freedSites = atoi64(m[1]), atoi64(m[2]), atoi64(m[3])
			section = "freed"
			continue
		}
		if strings.HasPrefix(line, "  ") && section != "" {
			if s := parseWinSite(line); s != nil {
				switch section {
				case "alive":
					blk.aliveLines = append(blk.aliveLines, *s)
				case "alloc":
					blk.allocLines = append(blk.allocLines, *s)
				case "freed":
					blk.freedLines = append(blk.freedLines, *s)
				}
			}
		}
	}
	return pd, sc.Err()
}

// ── Aggregation ───────────────────────────────────────────────────

type genAgg struct {
	gen            int
	reports        int
	allocObjs      int64
	allocBytes     int64
	freedObjs      int64
	freedBytes     int64
	finalAliveObjs int64 // alive of the gen's last report (0 if no alive section)
	finalAliveByte int64
	peakAliveObjs  int64
	peakAliveBytes int64
}

func aggregateGens(pd *parsedWindow) []*genAgg {
	var order []int
	byGen := map[int]*genAgg{}
	for _, b := range pd.blocks {
		g := byGen[b.gen]
		if g == nil {
			g = &genAgg{gen: b.gen}
			byGen[b.gen] = g
			order = append(order, b.gen)
		}
		g.reports++
		g.allocObjs += b.allocObjs
		g.allocBytes += b.allocBytes
		g.freedObjs += b.freedObjs
		g.freedBytes += b.freedBytes
		if b.hasAlive {
			g.finalAliveObjs = b.aliveObjs
			g.finalAliveByte = b.aliveBytes
			if b.aliveObjs > g.peakAliveObjs {
				g.peakAliveObjs = b.aliveObjs
			}
			if b.aliveBytes > g.peakAliveBytes {
				g.peakAliveBytes = b.aliveBytes
			}
		}
	}
	sort.Ints(order)
	out := make([]*genAgg, 0, len(order))
	for _, gen := range order {
		out = append(out, byGen[gen])
	}
	return out
}

type siteKey struct {
	gen int
	fn  string
	loc string
}

type siteAgg struct {
	key            siteKey
	stack          string // first seen full stack
	allocObjs      int64
	allocBytes     int64
	freedObjs      int64
	freedBytes     int64
	finalAliveObjs int64 // from the last report of this gen (0 if absent)
	finalAliveByte int64
}

func aggregateSites(pd *parsedWindow) []*siteAgg {
	sites := map[siteKey]*siteAgg{}
	var keys []siteKey
	get := func(gen int, s *winSite) *siteAgg {
		k := siteKey{gen: gen, fn: s.fn, loc: s.loc}
		a := sites[k]
		if a == nil {
			a = &siteAgg{key: k, stack: s.stack}
			sites[k] = a
			keys = append(keys, k)
		}
		return a
	}

	// Precompute the last block index per gen (for final alive attribution).
	lastBlockOfGen := map[int]int{}
	for i, b := range pd.blocks {
		lastBlockOfGen[b.gen] = i
	}

	for i, b := range pd.blocks {
		for j := range b.allocLines {
			a := get(b.gen, &b.allocLines[j])
			a.allocObjs += b.allocLines[j].objs
			a.allocBytes += b.allocLines[j].bytes
		}
		for j := range b.freedLines {
			a := get(b.gen, &b.freedLines[j])
			a.freedObjs += b.freedLines[j].objs
			a.freedBytes += b.freedLines[j].bytes
		}
		if lastBlockOfGen[b.gen] == i {
			// Final report of this gen: snapshot alive per site. Sites
			// absent here keep their zero value.
			for j := range b.aliveLines {
				a := get(b.gen, &b.aliveLines[j])
				a.finalAliveObjs = b.aliveLines[j].objs
				a.finalAliveByte = b.aliveLines[j].bytes
			}
		}
	}

	out := make([]*siteAgg, 0, len(keys))
	for _, k := range keys {
		out = append(out, sites[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.gen != out[j].key.gen {
			return out[i].key.gen < out[j].key.gen
		}
		return out[i].allocBytes > out[j].allocBytes
	})
	return out
}

// ── XLSX builder ──────────────────────────────────────────────────

type xlsxSheet struct {
	name      string
	rows      [][]string
	colWidths []float64
}

const maxColWidth = 70.0

func (s *xlsxSheet) addRow(cells ...string) {
	s.rows = append(s.rows, cells)
	for i, c := range cells {
		w := float64(len(c)) * 1.05
		if w < 10 {
			w = 10
		}
		if w > maxColWidth {
			w = maxColWidth
		}
		if w > s.colWidths[i] {
			s.colWidths[i] = w
		}
	}
}

func (s *xlsxSheet) addBlank() {
	s.rows = append(s.rows, nil)
}

func (s *xlsxSheet) addHeaderRow(cells ...string) {
	s.rows = append(s.rows, cells)
}

func escXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

func colLetter(i int) string {
	return string(rune('A' + i))
}

func fmtFloat(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprintf("%.1f", f)
}

func (xb *xlsxBuilder) writeXLSX(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	// [Content_Types].xml
	ct := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
  <Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>`
	for i := range xb.sheets {
		ct += fmt.Sprintf("\n  <Override PartName=\"/xl/worksheets/sheet%d.xml\" ContentType=\"application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml\"/>", i+1)
	}
	ct += "\n</Types>"
	xb.writeFile(w, "[Content_Types].xml", ct)

	// _rels/.rels
	rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`
	xb.writeFile(w, "_rels/.rels", rels)

	// xl/workbook.xml
	wb := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
          xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets>`
	for i, s := range xb.sheets {
		wb += fmt.Sprintf("\n    <sheet name=\"%s\" sheetId=\"%d\" r:id=\"rId%d\"/>", escXML(s.name), i+1, i+3)
	}
	wb += "\n  </sheets>\n</workbook>"
	xb.writeFile(w, "xl/workbook.xml", wb)

	// xl/_rels/workbook.xml.rels
	wbRels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>`
	for i := range xb.sheets {
		wbRels += fmt.Sprintf("\n  <Relationship Id=\"rId%d\" Type=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet\" Target=\"worksheets/sheet%d.xml\"/>", i+3, i+1)
	}
	wbRels += "\n</Relationships>"
	xb.writeFile(w, "xl/_rels/workbook.xml.rels", wbRels)

	// xl/styles.xml
	styles := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <fonts count="2">
    <font><sz val="11"/><name val="Consolas"/></font>
    <font><b/><sz val="11"/><color rgb="FFFFFFFF"/><name val="Consolas"/></font>
  </fonts>
  <fills count="3">
    <fill><patternFill patternType="none"/></fill>
    <fill><patternFill patternType="gray125"/></fill>
    <fill><patternFill patternType="solid"><fgColor rgb="FF4472C4"/></patternFill></fill>
  </fills>
  <borders count="2">
    <border><left/><right/><top/><bottom/><diagonal/></border>
    <border>
      <left style="thin"><color auto="1"/></left>
      <right style="thin"><color auto="1"/></right>
      <top style="thin"><color auto="1"/></top>
      <bottom style="thin"><color auto="1"/></bottom>
      <diagonal/>
    </border>
  </borders>
  <cellStyleXfs count="1">
    <xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>
  </cellStyleXfs>
  <cellXfs count="4">
    <xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>
    <xf numFmtId="0" fontId="1" fillId="2" borderId="1" xfId="0" applyFont="1" applyFill="1" applyBorder="1" applyAlignment="1"><alignment horizontal="center"/></xf>
    <xf numFmtId="0" fontId="0" fillId="0" borderId="1" xfId="0" applyBorder="1"/>
    <xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyFont="1"><font><b/><sz val="12"/><name val="Consolas"/></font></xf>
  </cellXfs>
</styleSheet>`
	xb.writeFile(w, "xl/styles.xml", styles)

	// xl/sharedStrings.xml
	ss := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="0" uniqueCount="0"></sst>`
	xb.writeFile(w, "xl/sharedStrings.xml", ss)

	// xl/worksheets/sheetN.xml
	for si, sheet := range xb.sheets {
		colXml := "<cols>"
		for i, w := range sheet.colWidths {
			if w > 0 {
				colXml += fmt.Sprintf("<col min=\"%d\" max=\"%d\" width=\"%.1f\" customWidth=\"1\"/>", i+1, i+1, w)
			}
		}
		colXml += "</cols>"

		rowsXml := ""
		inSection := false
		for ri, row := range sheet.rows {
			if row == nil {
				rowsXml += fmt.Sprintf("<row r=\"%d\"></row>", ri+1)
				inSection = false
				continue
			}
			// Determine style: header row (first or after blank) → style 1 (blue bold)
			// Section title (starts with "===" or "Mode:") → style 3 (bold 12pt)
			// Normal data → style 2 (border only)
			// Others → style 0
			styleIdx := 0
			if !inSection {
				styleIdx = 1
			} else {
				styleIdx = 2
			}
			if len(row) > 0 && (strings.HasPrefix(row[0], "==") || row[0] == "Mode:" || strings.HasPrefix(row[0], "Session #")) {
				styleIdx = 3
			}
			inSection = true

			rowsXml += fmt.Sprintf("<row r=\"%d\">", ri+1)
			for ci, cell := range row {
				ref := fmt.Sprintf("%s%d", colLetter(ci), ri+1)
				if cell == "" {
					rowsXml += fmt.Sprintf("<c r=\"%s\" s=\"%d\"/>", ref, styleIdx)
					continue
				}
				if n, err := strconv.Atoi(cell); err == nil {
					rowsXml += fmt.Sprintf("<c r=\"%s\" s=\"%d\"><v>%d</v></c>", ref, styleIdx, n)
				} else if f, err := strconv.ParseFloat(cell, 64); err == nil {
					rowsXml += fmt.Sprintf("<c r=\"%s\" s=\"%d\"><v>%s</v></c>", ref, styleIdx, fmtFloat(f))
				} else {
					rowsXml += fmt.Sprintf("<c r=\"%s\" t=\"inlineStr\" s=\"%d\"><is><t>%s</t></is></c>", ref, styleIdx, escXML(cell))
				}
			}
			rowsXml += "</row>"
		}

		sheetXml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
           xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  %s
  <sheetData>%s</sheetData>
</worksheet>`, colXml, rowsXml)

		xb.writeFile(w, fmt.Sprintf("xl/worksheets/sheet%d.xml", si+1), sheetXml)
	}

	return nil
}

type xlsxBuilder struct {
	sheets []xlsxSheet
}

func (xb *xlsxBuilder) writeFile(w *zip.Writer, name, content string) {
	f, _ := w.Create(name)
	f.Write([]byte(content))
}

// trimSheetName clamps a sheet name to Excel's 31-char limit and dedups.
func trimSheetName(name string, seen map[string]int) string {
	r := []rune(name)
	if len(r) > 31 {
		name = string(r[:31])
	}
	if n, ok := seen[name]; ok {
		suffix := fmt.Sprintf("~%d", n+1)
		r := []rune(name)
		if len(r)+len(suffix) > 31 {
			name = string(r[:31-len(suffix)])
		}
		name += suffix
	}
	seen[name] = 1
	return name
}

// ── build sheets from parsed data ─────────────────────────────────

func buildOverviewSheet(files []*parsedWindow) *xlsxSheet {
	s := &xlsxSheet{name: "Overview", colWidths: make([]float64, 11)}
	s.addHeaderRow("Mode", "Windows", "GC Reports", "Alloc Objs", "Alloc Bytes",
		"Freed Objs", "Freed Bytes", "Final Alive Objs", "Final Alive Bytes", "Peak Alive Objs", "Peak Alive Bytes")
	for _, pd := range files {
		gens := aggregateGens(pd)
		var ao, ab, fo, fb int64
		for _, g := range gens {
			ao += g.allocObjs
			ab += g.allocBytes
			fo += g.freedObjs
			fb += g.freedBytes
		}
		var fao, fab, pao, pab int64
		if len(gens) > 0 {
			last := gens[len(gens)-1]
			fao, fab = last.finalAliveObjs, last.finalAliveByte
		}
		for _, g := range gens {
			if g.peakAliveObjs > pao {
				pao = g.peakAliveObjs
			}
			if g.peakAliveBytes > pab {
				pab = g.peakAliveBytes
			}
		}
		s.addRow(pd.name, fmt.Sprint(len(gens)), fmt.Sprint(len(pd.blocks)),
			fmt.Sprint(ao), fmt.Sprint(ab), fmt.Sprint(fo), fmt.Sprint(fb),
			fmt.Sprint(fao), fmt.Sprint(fab), fmt.Sprint(pao), fmt.Sprint(pab))
	}
	return s
}

func buildWindowSheet(pd *parsedWindow) *xlsxSheet {
	s := &xlsxSheet{name: pd.name, colWidths: make([]float64, 11)}
	gens := aggregateGens(pd)

	var totAllocObjs, totAllocBytes, totFreedObjs, totFreedBytes int64
	for _, g := range gens {
		totAllocObjs += g.allocObjs
		totAllocBytes += g.allocBytes
		totFreedObjs += g.freedObjs
		totFreedBytes += g.freedBytes
	}

	s.addRow("Mode:", pd.name)
	s.addRow("GC Reports:", fmt.Sprint(len(pd.blocks)))
	s.addRow("Windows (gen):", fmt.Sprint(len(gens)))
	s.addRow("Total Alloc:", fmt.Sprintf("%d objs (%d bytes)", totAllocObjs, totAllocBytes))
	s.addRow("Total Freed:", fmt.Sprintf("%d objs (%d bytes)", totFreedObjs, totFreedBytes))
	s.addBlank()

	// === Summary === — per-gen aggregates.
	s.addRow("=== Summary ===")
	s.addHeaderRow("Gen", "GC Reports", "Alloc Objs", "Alloc Bytes",
		"Freed Objs", "Freed Bytes", "Final Alive Objs", "Final Alive Bytes",
		"Peak Alive Objs", "Peak Alive Bytes")
	for _, g := range gens {
		s.addRow(fmt.Sprint(g.gen), fmt.Sprint(g.reports),
			fmt.Sprint(g.allocObjs), fmt.Sprint(g.allocBytes),
			fmt.Sprint(g.freedObjs), fmt.Sprint(g.freedBytes),
			fmt.Sprint(g.finalAliveObjs), fmt.Sprint(g.finalAliveByte),
			fmt.Sprint(g.peakAliveObjs), fmt.Sprint(g.peakAliveBytes))
	}
	s.addBlank()

	// Data verification: per gen, Σalloc should equal Σfreed + final alive
	// (cycle counters are Xchg-cleared at each report, so Σ alloc sections
	// equals the final cumulative alloc count; alive = cumAllocs−cumFrees).
	// Small diffs can remain from racy increments landing mid-report.
	s.addRow("=== 数据验证 (per gen: alloc == freed + alive) ===")
	s.addHeaderRow("Gen", "Σ Alloc Objs", "Σ Freed + Alive Objs", "Diff Objs",
		"Σ Alloc Bytes", "Σ Freed + Alive Bytes", "Diff Bytes", "Verdict")
	for _, g := range gens {
		faObjs := g.freedObjs + g.finalAliveObjs
		faBytes := g.freedBytes + g.finalAliveByte
		diffObjs := g.allocObjs - faObjs
		diffBytes := g.allocBytes - faBytes
		verdict := "OK"
		if diffObjs != 0 || diffBytes != 0 {
			verdict = fmt.Sprintf("diff=%+d objs", diffObjs)
		}
		s.addRow(fmt.Sprint(g.gen), fmt.Sprint(g.allocObjs), fmt.Sprint(faObjs), fmt.Sprint(diffObjs),
			fmt.Sprint(g.allocBytes), fmt.Sprint(faBytes), fmt.Sprint(diffBytes), verdict)
	}
	s.addBlank()

	// === Per-GC Summary === — one row per report block.
	s.addRow("=== Per-GC Summary ===")
	s.addHeaderRow("Report", "GC #", "Gen", "Elapsed (s)",
		"Alloc Objs", "Alloc Bytes", "Freed Objs", "Freed Bytes",
		"Alive Objs", "Alive Bytes", "Alive Sites")
	for i, b := range pd.blocks {
		s.addRow(fmt.Sprint(i+1), fmt.Sprint(b.gcNum), fmt.Sprint(b.gen), fmt.Sprint(b.elapsed),
			fmt.Sprint(b.allocObjs), fmt.Sprint(b.allocBytes),
			fmt.Sprint(b.freedObjs), fmt.Sprint(b.freedBytes),
			fmt.Sprint(b.aliveObjs), fmt.Sprint(b.aliveBytes), fmt.Sprint(b.aliveSites))
	}
	return s
}

func buildWindowSitesSheet(pd *parsedWindow) *xlsxSheet {
	s := &xlsxSheet{name: pd.name + " - Sites", colWidths: make([]float64, 11)}
	sites := aggregateSites(pd)

	s.addRow("Mode:", pd.name)
	s.addRow("Note:", "Alloc/Freed are summed over all reports of the gen; Final Alive is from the gen's last report.")
	s.addBlank()

	s.addRow("=== Sites by Gen (sorted by alloc bytes) ===")
	s.addHeaderRow("Gen", "Function", "File:Line",
		"Alloc Objs", "Alloc Bytes", "Freed Objs", "Freed Bytes",
		"Final Alive Objs", "Final Alive Bytes", "Fully Dead", "Full Stack")
	for _, a := range sites {
		fullyDead := ""
		if a.allocObjs > 0 && a.finalAliveObjs == 0 {
			fullyDead = "YES"
		}
		s.addRow(fmt.Sprint(a.key.gen), a.key.fn, a.key.loc,
			fmt.Sprint(a.allocObjs), fmt.Sprint(a.allocBytes),
			fmt.Sprint(a.freedObjs), fmt.Sprint(a.freedBytes),
			fmt.Sprint(a.finalAliveObjs), fmt.Sprint(a.finalAliveByte),
			fullyDead, a.stack)
	}
	return s
}

// ── main ──────────────────────────────────────────────────────────

func unique(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func main() {
	inputs := os.Args[1:]
	if len(inputs) == 0 {
		for _, dir := range []string{".", "output"} {
			for _, pat := range []string{"gcdeadwindow_*.log", "gcdeadwindow_*.txt"} {
				matches, _ := filepath.Glob(filepath.Join(dir, pat))
				inputs = append(inputs, matches...)
			}
		}
		inputs = unique(inputs)
	}
	if len(inputs) == 0 {
		fmt.Println("no gcdeadwindow log files found (usage: excel_report_window [file1.log ...])")
		os.Exit(1)
	}

	var files []*parsedWindow
	for _, path := range inputs {
		pd, err := parseWindowFile(path)
		if err != nil {
			fmt.Printf("skip %s: %v\n", path, err)
			continue
		}
		if len(pd.blocks) == 0 {
			fmt.Printf("skip %s: no gcdeadwindow GC blocks found\n", path)
			continue
		}
		files = append(files, pd)
	}
	if len(files) == 0 {
		fmt.Println("no parseable gcdeadwindow data")
		os.Exit(1)
	}

	builder := xlsxBuilder{}
	builder.sheets = append(builder.sheets, *buildOverviewSheet(files))

	seen := map[string]int{"Overview": 1}
	for _, pd := range files {
		main1 := buildWindowSheet(pd)
		main1.name = trimSheetName(main1.name, seen)
		builder.sheets = append(builder.sheets, *main1)

		sites := buildWindowSitesSheet(pd)
		sites.name = trimSheetName(sites.name, seen)
		builder.sheets = append(builder.sheets, *sites)
	}

	out := "gcdeadwindow_report.xlsx"
	if err := builder.writeXLSX(out); err != nil {
		fmt.Printf("write %s: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d files, %d sheets)\n", out, len(files), len(builder.sheets))
}
