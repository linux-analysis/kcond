package analysis

import (
	"cmp"
	"context"
	"path"
	"slices"
	"strings"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/builds"
	"github.com/linux-analysis/kcond/change"
	"github.com/linux-analysis/kcond/gitfs"
	"github.com/linux-analysis/kcond/kernel"
	"github.com/linux-analysis/kcond/unidiff"
)

// FileResult is a file a commit changes, with its conditions.
type FileResult struct {
	Path      string            `json:"path"`
	Header    bool              `json:"header"`
	Region    string            `json:"region"`
	Functions []string          `json:"functions,omitempty"`
	Rev       string            `json:"rev,omitempty"`
	Status    string            `json:"status"`
	Kbuild    string            `json:"kbuild,omitempty"`
	Builds    map[string]string `json:"builds,omitempty"`
	Arches    []string          `json:"arches"`
	Unknown   []string          `json:"unknown_arches"`
}

// CommitResult is what Diff reports for one commit.
type CommitResult struct {
	Commit    string       `json:"commit"`
	Files     []FileResult `json:"files"`
	Condition string       `json:"condition"`
	Label     string       `json:"label"`
	Arches    []string     `json:"arches"`
	Verdict   string       `json:"verdict,omitempty"`
}

// Diff reads the commits args name (see Walk): the kernel files each one
// changes, the region of the changed lines, each file's condition at the
// revisions Walk gives, the condition of the whole change and its arch
// label. o.Build is the only build read, without the arch label and the
// architectures; without one every build is, and the condition is
// x86-64's. With o.Config each commit has its verdict.
func Diff(ctx context.Context, dir string, args []string, o Options) ([]CommitResult, error) {
	cat, err := gitfs.NewCatFile(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer cat.Close()
	// Every commit's files go through one crawl, which reads each
	// revision once.
	var commits []Commit
	var changes [][]change.FileChange
	var counted [][]bool  // changes[i][j] counts in its commit (see builds.Counted)
	var others [][]string // when no file of the commit counts, the files it changes the build reads
	var files []*File
	var readErr error
	err = Walk(ctx, dir, args, o.Warn, func(c Commit, d string) {
		all := change.Changes(cat, c.Hash, d)
		// A file whose changed lines are all comments decides nothing.
		code := slices.DeleteFunc(slices.Clone(all), func(ch change.FileChange) bool { return !ch.Code })
		commits = append(commits, c)
		changes = append(changes, code)
		sources := slices.ContainsFunc(code, func(ch change.FileChange) bool { return !ch.Header })
		counts := make([]bool, len(code))
		for j, ch := range code {
			f := &File{File: kernel.File{Path: ch.Path, Header: ch.Header}, Region: ch.Region, Revs: c.Revs}
			terms := builds.Counted(f.File, ch.Terms, sources)
			if len(terms) > 0 && len(terms) < len(ch.Terms) {
				f.Count = kcond.Or(terms...)
			}
			counts[j] = len(terms) > 0
			files = append(files, f)
		}
		counted = append(counted, counts)
		var read []string
		if !slices.Contains(counts, true) && readErr == nil {
			read, readErr = ReadByBuild(ctx, dir, c.Hash, d, all)
		}
		others = append(others, read)
	})
	if err == nil {
		err = readErr
	}
	if err != nil {
		return nil, err
	}
	build := o.Build
	found, err := Resolve(ctx, dir, files, Options{Build: build})
	if err != nil {
		return nil, err
	}
	results := make([]CommitResult, 0, len(commits))
	for i, c := range commits {
		r := CommitResult{Commit: c.Hash, Files: []FileResult{}}
		var terms []*kcond.Expr
		var fas []builds.FileArches
		for j, ch := range changes[i] {
			res := found[0]
			found = found[1:]
			// The region is written as the build reads it, like the
			// condition: X=y as X for a bool, MODULE and -D macros
			// resolved. A revision without the build keeps the changed
			// lines' region.
			fc, region := res.Cond(build), ch.Region
			if _, ok := res.Conds[cmp.Or(build, kernel.X86_64Build)]; ok {
				region = fc.Region
			}
			c := FileResult{Path: ch.Path, Header: ch.Header, Region: region.String(), Functions: ch.Functions, Rev: res.Rev,
				Status: fc.Status.String(), Kbuild: condText(fc.Kbuild)}
			c.Arches, c.Unknown = res.Arches.Lists()
			for name, b := range res.Conds {
				if name == kernel.X86_64Build {
					continue // the file's row shows it
				}
				if c.Builds == nil {
					c.Builds = map[string]string{}
				}
				c.Builds[name] = b.Status.String()
				if b.Kbuild != nil {
					c.Builds[name] += " " + b.Kbuild.String()
				}
			}
			r.Files = append(r.Files, c)
			if counted[i][j] {
				terms = append(terms, builds.Term(res.Counted(build)))
				fas = append(fas, res.CountedArches())
			}
		}
		// Without a kernel file that counts, the other files the build
		// reads, as Makefiles, Kconfig and device trees, are unknown.
		for _, p := range others[i] {
			terms = append(terms, builds.Unknown(p))
		}
		e := kcond.Or(terms...)
		sum := builds.Summarize(fas)
		if len(terms) == 0 {
			sum.Label = "none" // the build reads nothing the commit changes
		}
		r.Condition, r.Label, r.Arches = e.String(), sum.Label, sum.Arches
		if o.Config != nil {
			r.Verdict = change.VerdictName(e.Eval(o.Config))
		}
		results = append(results, r)
	}
	return results, nil
}

// ReadByBuild returns the files a diff changes that the build reads: those
// not outside it (see kernel.OutsideBuild), except a kernel file whose
// changed lines are all comments, as changes tells, and a file under
// scripts/ that nothing names at rev (see namedScripts).
func ReadByBuild(ctx context.Context, dir, rev, diff string, changes []change.FileChange) ([]string, error) {
	comments := map[string]bool{}
	for _, ch := range changes {
		comments[ch.Path] = !ch.Code
	}
	var read []string
	for _, f := range unidiff.Files(diff) {
		for _, p := range []string{f.OldPath, f.NewPath} {
			if p != "" && !kernel.OutsideBuild(p) && !comments[p] && !slices.Contains(read, p) {
				read = append(read, p)
			}
		}
	}
	return namedScripts(ctx, dir, rev, read)
}

// namedScripts drops from paths the files under scripts/ the build never
// runs: those no Kbuild file and no other file under scripts/ names, by
// their base name without extension as a whole word, as hostprogs-y +=
// sorttable names scripts/sorttable.c and link-vmlinux.sh names mksysmap.
// A semantic patch, which coccicheck runs through a glob, is named by
// nothing. Kbuild files under scripts/ are read by the build and stay.
func namedScripts(ctx context.Context, dir, rev string, paths []string) ([]string, error) {
	stems := map[string]string{}
	var words []string
	for _, p := range paths {
		if b := path.Base(p); strings.HasPrefix(p, "scripts/") && !strings.Contains(b, "Makefile") && !strings.Contains(b, "Kbuild") {
			if ext := path.Ext(b); ext != b {
				b = strings.TrimSuffix(b, ext)
			}
			stems[p] = b
			words = append(words, b)
		}
	}
	if len(words) == 0 {
		return paths, nil
	}
	lines, err := gitfs.GrepLines(ctx, dir, rev, words, "*Makefile*", "*Kbuild*", "scripts/")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(paths, func(p string) bool {
		stem, ok := stems[p]
		if !ok {
			return false
		}
		for file, ls := range lines {
			if file != p && !kernel.OutsideBuild(file) && slices.ContainsFunc(ls, func(l string) bool { return hasWord(l, stem) }) {
				return false
			}
		}
		return true
	}), nil
}

// hasWord tells whether s holds w with no letter, digit or _ on either
// side, as git grep -w does.
func hasWord(s, w string) bool {
	word := func(b byte) bool {
		return b == '_' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
	}
	for i := 0; ; i++ {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		i += j
		if (i == 0 || !word(s[i-1])) && (i+len(w) == len(s) || !word(s[i+len(w)])) {
			return true
		}
	}
}

// condText writes e, "" for nil.
func condText(e *kcond.Expr) string {
	if e == nil {
		return ""
	}
	return e.String()
}
