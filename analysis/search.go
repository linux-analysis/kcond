package analysis

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"slices"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/builds"
	"github.com/linux-analysis/kcond/change"
	"github.com/linux-analysis/kcond/gitfs"
	"github.com/linux-analysis/kcond/kernel"
	"github.com/linux-analysis/kcond/unidiff"
)

// Match is one commit Search found: the first changed file that passed
// the filters and the condition it passed with.
type Match struct {
	Commit    string   `json:"commit"`
	Path      string   `json:"path"`
	Condition string   `json:"condition"`
	Label     string   `json:"label,omitempty"`
	Arches    []string `json:"arches,omitempty"`
}

// Filters are what Search keeps a commit for. Config and Where hold when
// some changed line passes them, Always and Arch some changed file, or
// every one with Strict.
type Filters struct {
	Config kcond.Config // a changed line is built by the .config
	Where  *kcond.Expr  // a changed line is built only where it holds (see builds.Build.Implies)
	Always bool         // a changed file is built in every configuration, its lines together
	Arch   string       // the arch may build a changed file
	Label  string       // the change has this arch label
	Strict bool
}

// counted returns the conditions of a file's changed lines that count
// (see builds.Counted), the lines the filters judge one by one; Always
// reads them together, as diff does, so a header changing both branches
// of an #ifdef is built in every configuration.
func (fl Filters) counted(f kernel.File, terms []*kcond.Expr, sources bool) []*kcond.Expr {
	terms = builds.Counted(f, terms, sources)
	if fl.Always && len(terms) > 1 {
		return []*kcond.Expr{kcond.Or(terms...)}
	}
	return terms
}

// test tells whether the line filters hold for a changed line of f whose
// raw region is region and condition in b is e (see builds.Term), which
// fails them when unknown: False when it fails them, True when it passes.
// With whole, e is the file's with the region true and the answer is for
// all of its lines: False when none can pass, True when every one does,
// Unknown when their regions must be read.
func (fl Filters) test(b builds.Build, f kernel.File, region, e *kcond.Expr, whole bool) kcond.Tri {
	if builds.IsUnknown(e) || b.Possible(e) == kcond.False {
		return kcond.False
	}
	res := kcond.True
	if fl.Config != nil {
		if e.Eval(fl.Config) != kcond.True {
			return kcond.False // a region only narrows the condition
		}
		if whole {
			res = kcond.Unknown
		}
	}
	if fl.Where != nil && b.Implies(e, fl.Where) != kcond.True {
		if !whole {
			return kcond.False
		}
		res = kcond.Unknown // a region may add what the Kbuild condition lacks
	}
	if fl.Always {
		if !b.AlwaysBuilds(b.Eval(f, region)) {
			return kcond.False
		}
		if whole {
			res = kcond.Unknown
		}
	}
	return res
}

// Search lists the commits args name (see Walk) whose change passes the
// filters, oldest first; o.Build is the build read, x86-64 when "", and o.Warn
// hears of the builds that could not be read. A commit's
// files are judged at the revision diff reads their Makefiles at, from
// their Kbuild condition alone where that settles the filters and from the
// regions of their changed lines otherwise, the first line that decides
// the commit ending its reading; a file that revision lacks, and the
// architectures, are resolved for the surviving commits afterwards.
func Search(ctx context.Context, dir string, args []string, fl Filters, o Options) ([]Match, error) {
	lines := fl.Config != nil || fl.Where != nil || fl.Always // the filters on changed lines
	arches := fl.Arch != "" || fl.Label != ""                 // the filters on architectures
	cat, err := gitfs.NewCatFile(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer cat.Close()
	build := cmp.Or(o.Build, kernel.X86_64Build)
	// The build is read at a range's end, which the range's commits share;
	// a revision without the build leaves their files to Resolve, as does
	// a commit named alone, which reads its own revision.
	var tree *gitfs.Tree
	var b builds.Build
	at, built := "", false
	defer func() {
		if tree != nil {
			tree.Close()
		}
	}()
	openAt := func(rev string) error {
		if tree != nil {
			tree.Close()
			tree = nil
		}
		at = rev
		t, err := gitfs.Open(ctx, dir, rev)
		if err != nil {
			return err
		}
		tree = t
		if b, built = builds.NewSession(tree, nil).Build(build); built {
			warn(o.Warn, b)
		}
		return nil
	}
	// A commit the stream could not settle: its files the build's revision
	// lacks, or its architectures, wait for one Resolve at the end.
	type pending struct {
		m     Match
		files []*File // the code files, with the region of the lines that count, in diff order
		hit   bool    // a line passed the line filters
	}
	// A file whose lines the line filters wait for: the conditions of those
	// that count, and what the build of the revision Resolve reads the
	// file at says of them (see visit).
	type waiting struct {
		terms  []*kcond.Expr
		judged bool   // the build read the file
		pass   string // the condition of the first line that passed
		fail   bool   // a line failed
	}
	var found []*pending
	var all []*File
	waits := map[*File]*waiting{}
	var firstErr error
	err = Walk(ctx, dir, args, o.Warn, func(c Commit, d string) {
		if firstErr != nil {
			return
		}
		alone := c.Revs[0] == c.Hash
		if !alone && at != c.Revs[0] {
			if firstErr = openAt(c.Revs[0]); firstErr != nil {
				return
			}
		}
		later := alone || !built // every file waits for Resolve
		var fds []unidiff.FileDiff
		for _, fd := range unidiff.Files(d) {
			p := cmp.Or(fd.OldPath, fd.NewPath)
			if _, ok := kernel.FileOf(p); ok && len(fd.Hunks) > 0 {
				fds = append(fds, fd)
			}
		}
		// Each file's regions are read once, at the first need.
		regions := make([]*change.FileChange, len(fds))
		region := func(i int) *change.FileChange {
			if regions[i] == nil {
				fc, _ := change.Change(cat, c.Hash, fds[i])
				regions[i] = &fc
			}
			return regions[i]
		}
		// sources tells whether a source of the commit changes code, which
		// decides which lines of a header count (see builds.Counted); it
		// reads regions, so only headers ask it.
		srcRead, srcs := false, false
		sources := func() bool {
			if !srcRead {
				srcRead = true
				for i, fd := range fds {
					if f, _ := kernel.FileOf(cmp.Or(fd.OldPath, fd.NewPath)); !f.Header && region(i).Code {
						srcs = true
						break
					}
				}
			}
			return srcs
		}
		pc := &pending{m: Match{Commit: c.Hash}}
		var missing []int
		rejected := false
		for i, fd := range fds {
			if !lines || rejected || (pc.hit && !fl.Strict) {
				break
			}
			p := cmp.Or(fd.OldPath, fd.NewPath)
			f, _ := kernel.FileOf(p)
			header := f.Header
			if later {
				missing = append(missing, i)
				continue
			}
			if _, err := fs.Stat(tree, p); err != nil {
				missing = append(missing, i)
				continue
			}
			whole := kcond.Const(true)
			kf := kernel.File{Path: p, Header: header}
			fcw := b.Eval(kf, whole)
			// Next to a source, a header counts through some of its
			// lines only, as does one no includer resolves: its regions
			// decide.
			some := header && sources()
			t := kcond.Unknown
			if fcw.Status != builds.StatusHeader && !some {
				t = fl.test(b, kf, whole, builds.Term(fcw), true)
			}
			if t == kcond.False && !fl.Strict {
				continue // no line of the file passes
			}
			fc := region(i)
			if !fc.Code {
				continue // comments only: no line
			}
			if t == kcond.False {
				rejected = true
				break
			}
			terms := fl.counted(kf, fc.Terms, some)
			if len(terms) == 0 {
				continue // no line of the header counts
			}
			if t == kcond.True && !fl.Strict {
				// Every line passes but one never built, which --strict
				// must see: the file's condition.
				terms = []*kcond.Expr{fc.Region}
			}
			for _, term := range terms {
				e := builds.Term(b.Eval(kf, term))
				switch fl.test(b, kf, term, e, false) {
				case kcond.True:
					if !pc.hit {
						pc.hit, pc.m.Path, pc.m.Condition = true, fc.Path, e.String()
					}
				case kcond.False:
					if fl.Strict {
						rejected = true
					}
				}
				if rejected || (pc.hit && !fl.Strict) {
					break
				}
			}
		}
		if rejected || (lines && !pc.hit && len(missing) == 0) {
			return // a line failed, or none passed and none is left to read
		}
		// The files Resolve must read: every code file for the arches, else
		// the missing ones while they can still decide the line filters.
		want := missing
		if arches {
			want = make([]int, len(fds))
			for i := range fds {
				want[i] = i
			}
		} else if pc.hit && !fl.Strict {
			want = nil
		}
		for _, i := range want {
			fc := region(i)
			if !fc.Code {
				continue
			}
			kf := kernel.File{Path: fc.Path, Header: fc.Header}
			terms := fl.counted(kf, fc.Terms, fc.Header && sources())
			if len(terms) == 0 {
				continue // no line of the header counts
			}
			wait := lines && slices.Contains(missing, i)
			revs := c.Revs
			if wait && !later {
				revs = revs[1:] // the range's end lacks the file
			}
			f := &File{File: kf, Region: kcond.Or(terms...), Revs: revs}
			if wait {
				waits[f] = &waiting{terms: terms}
			}
			pc.files = append(pc.files, f)
		}
		if lines && !pc.hit && !slices.ContainsFunc(pc.files, func(f *File) bool { return waits[f] != nil }) {
			return // the files left change no code
		}
		if arches && len(pc.files) == 0 {
			// As diff has it: the other files the build reads are unknown.
			var chs []change.FileChange
			for i := range fds {
				chs = append(chs, *region(i))
			}
			var terms []*kcond.Expr
			for _, p := range ReadByBuild(d, chs) {
				terms = append(terms, builds.Unknown(p))
			}
			if len(terms) == 0 {
				pc.m.Label = "none"
			}
			pc.m.Condition = kcond.Or(terms...).String()
		}
		all = append(all, pc.files...)
		found = append(found, pc)
	})
	if err == nil {
		err = firstErr
	}
	if err != nil {
		return nil, err
	}
	resolveBuild := build
	if arches {
		resolveBuild = ""
	}
	// A file waiting for the line filters is judged a line at a time by
	// the build of its revision.
	visit := func(f *File, b builds.Build, _ builds.FileCond) {
		w := waits[f]
		if w == nil || b.Name != build {
			return
		}
		w.judged = true
		for _, t := range w.terms {
			e := builds.Term(b.Eval(f.File, t))
			switch fl.test(b, f.File, t, e, false) {
			case kcond.True:
				w.pass = cmp.Or(w.pass, e.String())
			case kcond.False:
				w.fail = true
			}
		}
	}
	results, err := Resolve(ctx, dir, all, Options{Build: resolveBuild, Visit: visit})
	if err != nil {
		return nil, err
	}
	matches := []Match{}
	for _, pc := range found {
		res := results[:len(pc.files)]
		results = results[len(pc.files):]
		rejected := false
		var fas []builds.FileArches
		for i, f := range pc.files {
			fc := res[i].Cond(build)
			fas = append(fas, res[i].Arches)
			e := builds.Term(fc)
			if w := waits[f]; w != nil && (!pc.hit || fl.Strict) {
				if w.pass != "" && !pc.hit {
					pc.hit, pc.m.Path, pc.m.Condition = true, f.Path, w.pass
				}
				// A file no build read has no line that passes.
				rejected = rejected || fl.Strict && (w.fail || !w.judged)
			}
			// Without line filters the first file --arch may build, or
			// any arch, decides, with its condition in that arch's build.
			may, _ := res[i].Arches.Lists()
			a := fl.Arch
			if a == "" && len(may) > 0 {
				a = may[0]
			}
			has := slices.Contains(may, a)
			if fl.Arch != "" && fl.Strict && !has {
				rejected = true
			}
			if !lines && has && pc.m.Path == "" {
				if a != kernel.Primary {
					e = builds.Term(res[i].Cond(a)) // the other arches' builds bear their names
				}
				pc.m.Path, pc.m.Condition = f.Path, e.String()
			}
		}
		// A change no arch builds, as --label none or other finds, has its
		// first file, as diff reads it.
		if !lines && pc.m.Path == "" && len(pc.files) > 0 {
			pc.m.Path, pc.m.Condition = pc.files[0].Path, builds.Term(res[0].Cond(build)).String()
		}
		if rejected || (lines && !pc.hit) {
			continue
		}
		if arches {
			sum := builds.Summarize(fas)
			pc.m.Label, pc.m.Arches = cmp.Or(sum.Label, pc.m.Label), sum.Arches
			if fl.Label != "" && pc.m.Label != fl.Label {
				continue
			}
			if fl.Arch != "" && !slices.Contains(sum.Arches, fl.Arch) {
				continue
			}
		}
		matches = append(matches, pc.m)
	}
	return matches, nil
}

// warn tells w why build b's Makefiles or Kconfig could not be read, which
// leaves its files unknown.
func warn(w func(string), b builds.Build) {
	if b.Err != nil && w != nil {
		w(fmt.Sprintf("the %s build: %v", b.Name, b.Err))
	}
}
