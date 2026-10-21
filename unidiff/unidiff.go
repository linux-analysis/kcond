// Package unidiff reads git unified diffs into files and hunks.
package unidiff

import (
	"slices"
	"strconv"
	"strings"
)

// FileDiff is the part of a git unified diff that changes one file. A path
// is empty when the file is created or deleted.
type FileDiff struct {
	OldPath, NewPath string
	Hunks            []Hunk
}

// Hunk is one @@ section. Deleted holds the old line numbers of deleted
// lines and Added the new line numbers of added lines. A merge's combined
// diff gives one Hunk per parent for each @@@ section: Parent is the
// parent's index, from 0, whose lines OldStart, OldLines and Deleted
// count, and Added holds the new lines that parent lacks.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Func               string
	Deleted            []int
	Added              []int
	Parent             int
}

// Files splits a git diff, plain or combined, into files and hunks.
// Malformed parts are skipped.
func Files(diff string) []FileDiff {
	var files []FileDiff
	var f *FileDiff
	// hs is the section being read, one hunk per parent, with the next
	// line number and the lines left of each parent and of the new file.
	var hs []Hunk
	var oldLine, oldLeft []int
	newLine, newLeft := 0, 0
	for line := range strings.SplitSeq(diff, "\n") {
		if hs != nil && (newLeft > 0 || slices.ContainsFunc(oldLeft, func(n int) bool { return n > 0 })) {
			// The first column per parent marks the line: - when that
			// parent has it and the new file does not, + when the new file
			// has it and that parent does not.
			cols := line[:min(len(hs), len(line))]
			minus, bad := false, false
			for i := range len(cols) {
				switch cols[i] {
				case '-':
					minus = true
				case ' ', '+':
				default:
					bad = true
				}
			}
			switch {
			case strings.HasPrefix(line, "\\"):
			case bad:
				hs = nil
			case minus:
				for i := range len(cols) {
					if cols[i] == '-' {
						hs[i].Deleted = append(hs[i].Deleted, oldLine[i])
						oldLine[i], oldLeft[i] = oldLine[i]+1, oldLeft[i]-1
					}
				}
			default:
				for i := range hs {
					if i < len(cols) && cols[i] == '+' {
						hs[i].Added = append(hs[i].Added, newLine)
					} else {
						oldLine[i], oldLeft[i] = oldLine[i]+1, oldLeft[i]-1
					}
				}
				newLine, newLeft = newLine+1, newLeft-1
			}
			if hs != nil {
				continue
			}
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, FileDiff{})
			f, hs = &files[len(files)-1], nil
			if a, b, ok := strings.Cut(strings.TrimPrefix(line, "diff --git "), " b/"); ok {
				f.OldPath, f.NewPath = diffPath(a), b
			}
		case strings.HasPrefix(line, "diff --cc "):
			p := strings.TrimPrefix(line, "diff --cc ")
			if u, err := strconv.Unquote(p); err == nil {
				p = u
			}
			files = append(files, FileDiff{OldPath: p, NewPath: p})
			f, hs = &files[len(files)-1], nil
		case f == nil:
		case strings.HasPrefix(line, "rename from "):
			f.OldPath = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			f.NewPath = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "--- "):
			f.OldPath = diffPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			f.NewPath = diffPath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "@@"):
			sec, ok := parseHunkHeader(line)
			if !ok {
				hs = nil
				continue
			}
			f.Hunks = append(f.Hunks, sec...)
			hs = f.Hunks[len(f.Hunks)-len(sec):]
			oldLine, oldLeft = oldLine[:0], oldLeft[:0]
			for _, h := range sec {
				oldLine, oldLeft = append(oldLine, h.OldStart), append(oldLeft, h.OldLines)
			}
			newLine, newLeft = sec[0].NewStart, sec[0].NewLines
		}
	}
	return files
}

// diffPath strips the a/ or b/ prefix and maps /dev/null to "".
func diffPath(p string) string {
	if p == "/dev/null" {
		return ""
	}
	if u, err := strconv.Unquote(p); err == nil {
		p = u
	}
	if len(p) > 2 && (p[:2] == "a/" || p[:2] == "b/") {
		return p[2:]
	}
	return p
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@ func", or the header of a
// merge's combined diff, which has one more @ and one more -a,b per
// further parent, into one hunk per parent.
func parseHunkHeader(line string) ([]Hunk, bool) {
	at := line[:len(line)-len(strings.TrimLeft(line, "@"))]
	ranges, fn, ok := strings.Cut(strings.TrimPrefix(line, at+" "), " "+at)
	fields := strings.Fields(ranges)
	if !ok || len(at) < 2 || len(fields) != len(at) || !strings.HasPrefix(fields[len(fields)-1], "+") {
		return nil, false
	}
	newStart, newLines, ok := parseRange(fields[len(fields)-1][1:])
	if !ok {
		return nil, false
	}
	hs := make([]Hunk, len(at)-1)
	for i, o := range fields[:len(hs)] {
		h := Hunk{NewStart: newStart, NewLines: newLines, Func: strings.TrimSpace(fn), Parent: i}
		if !strings.HasPrefix(o, "-") {
			return nil, false
		}
		if h.OldStart, h.OldLines, ok = parseRange(o[1:]); !ok {
			return nil, false
		}
		hs[i] = h
	}
	return hs, true
}

func parseRange(s string) (start, count int, ok bool) {
	a, b, hasCount := strings.Cut(s, ",")
	start, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, false
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(b); err != nil {
			return 0, 0, false
		}
	}
	return start, count, true
}
