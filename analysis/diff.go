package analysis

import (
	"cmp"
	"context"
	"slices"

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
	Unknown   bool         `json:"unknown"`
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
	var counted [][]bool // changes[i][j] counts in its commit (see builds.Counted)
	var unread []bool    // the commit changes nothing the build reads
	var files []*File
	err = Walk(ctx, dir, args, o.Warn, func(c Commit, d string) {
		all := change.Changes(cat, c.Hash, d)
		// A file whose changed lines are all comments decides nothing.
		code := slices.DeleteFunc(slices.Clone(all), func(ch change.FileChange) bool { return !ch.Code })
		commits = append(commits, c)
		changes = append(changes, code)
		unread = append(unread, len(code) == 0 && Unread(d, all))
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
	})
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
		r := CommitResult{Commit: c.Hash}
		var conds []builds.FileCond
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
				conds = append(conds, res.Counted(build))
				fas = append(fas, res.CountedArches())
			}
		}
		e, unknown := builds.Combine(conds)
		sum := builds.Summarize(fas)
		if unread[i] {
			e, unknown, sum.Label = kcond.Const(false), false, "none"
		}
		r.Condition, r.Unknown = condText(e), unknown
		r.Label, r.Arches = sum.Label, sum.Arches
		if o.Config != nil {
			r.Verdict = change.VerdictName(change.Verdict(e, unknown, o.Config))
		}
		results = append(results, r)
	}
	return results, nil
}

// Unread reports whether the build reads none of the files a diff
// changes: each is outside the build, or a kernel file whose changed lines
// are all comments, as changes tells.
func Unread(diff string, changes []change.FileChange) bool {
	comments := map[string]bool{}
	for _, ch := range changes {
		comments[ch.Path] = !ch.Code
	}
	for _, f := range unidiff.Files(diff) {
		for _, p := range []string{f.OldPath, f.NewPath} {
			if p != "" && !kernel.OutsideBuild(p) && !comments[p] {
				return false
			}
		}
	}
	return true
}

// condText writes e, "" for nil.
func condText(e *kcond.Expr) string {
	if e == nil {
		return ""
	}
	return e.String()
}
