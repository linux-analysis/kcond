// Package unidiff reads git unified diffs into files and hunks.
package unidiff

import (
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
// lines and Added the new line numbers of added lines.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Func               string
	Deleted            []int
	Added              []int
}

// Files splits a git diff into files and hunks. Malformed parts are
// skipped.
func Files(diff string) []FileDiff {
	var files []FileDiff
	var f *FileDiff
	var h *Hunk
	oldLeft, newLeft, oldLine, newLine := 0, 0, 0, 0
	for line := range strings.SplitSeq(diff, "\n") {
		if h != nil && (oldLeft > 0 || newLeft > 0) {
			switch {
			case line == "" || line[0] == ' ':
				oldLine, newLine = oldLine+1, newLine+1
				oldLeft, newLeft = oldLeft-1, newLeft-1
			case line[0] == '-':
				h.Deleted = append(h.Deleted, oldLine)
				oldLine, oldLeft = oldLine+1, oldLeft-1
			case line[0] == '+':
				h.Added = append(h.Added, newLine)
				newLine, newLeft = newLine+1, newLeft-1
			case line[0] == '\\':
			default:
				h, oldLeft, newLeft = nil, 0, 0
			}
			if h != nil {
				continue
			}
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, FileDiff{})
			f, h = &files[len(files)-1], nil
			if a, b, ok := strings.Cut(strings.TrimPrefix(line, "diff --git "), " b/"); ok {
				f.OldPath, f.NewPath = diffPath(a), b
			}
		case f == nil:
		case strings.HasPrefix(line, "rename from "):
			f.OldPath = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			f.NewPath = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "--- "):
			f.OldPath = diffPath(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			f.NewPath = diffPath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "@@ "):
			hk, ok := parseHunkHeader(line)
			if !ok {
				h = nil
				continue
			}
			f.Hunks = append(f.Hunks, hk)
			h = &f.Hunks[len(f.Hunks)-1]
			oldLeft, newLeft = hk.OldLines, hk.NewLines
			oldLine, newLine = hk.OldStart, hk.NewStart
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

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@ func".
func parseHunkHeader(line string) (Hunk, bool) {
	rest := strings.TrimPrefix(line, "@@ ")
	ranges, fn, ok := strings.Cut(rest, " @@")
	if !ok {
		return Hunk{}, false
	}
	o, n, ok := strings.Cut(ranges, " ")
	if !ok || !strings.HasPrefix(o, "-") || !strings.HasPrefix(n, "+") {
		return Hunk{}, false
	}
	var h Hunk
	if h.OldStart, h.OldLines, ok = parseRange(o[1:]); !ok {
		return Hunk{}, false
	}
	if h.NewStart, h.NewLines, ok = parseRange(n[1:]); !ok {
		return Hunk{}, false
	}
	h.Func = strings.TrimSpace(fn)
	return h, true
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
