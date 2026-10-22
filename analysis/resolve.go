// Package analysis answers what kcond asks of commits: Walk lists the
// commits arguments name with their diffs, and Resolve finds the Kbuild
// conditions and the architectures of their files at the revisions of a
// git tree, crawling each revision once for all the files that need it
// and looking a file a revision lacks up at its next one.
package analysis

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"sync"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/builds"
	"github.com/linux-analysis/kcond/gitfs"
	"github.com/linux-analysis/kcond/kbuild"
	"github.com/linux-analysis/kcond/kernel"
)

// File is a changed file to look up.
type File struct {
	kernel.File
	Region *kcond.Expr // the condition of the changed lines
	// Count is the condition of the changed lines that count in the
	// change (see builds.Counted), when it is not Region.
	Count *kcond.Expr
	Revs  []string // the revisions to read the Makefiles from, in order
}

// Result is what the crawl found for a file.
type Result struct {
	// Rev is the revision whose Makefiles were read: the first of the
	// file's Revs that has it, or "" when none does.
	Rev string
	// Conds holds the file's conditions in the builds read at Rev, by
	// name, and counts those of its lines that count in the change, when
	// the file has a Count; countArches are the arches of those lines.
	Conds       map[string]builds.FileCond
	Arches      builds.FileArches
	counts      map[string]builds.FileCond
	count       *kcond.Expr
	countArches builds.FileArches

	// before is what is known of the file without a revision: a header
	// whose includers are not known has the status header, one of another
	// arch than the primary one is not built, and a source no revision has
	// is missing.
	before builds.FileCond
}

// Cond is the file's condition in build, x86-64's for "". Without a
// revision that has the file, or without its x86-64 build, it is what is
// known of the file without one; a revision without another build leaves
// the file unlisted there.
func (r Result) Cond(build string) builds.FileCond {
	if build == "" {
		build = kernel.X86_64Build
	}
	if fc, ok := r.Conds[build]; ok {
		return fc
	}
	if r.Rev == "" || build == kernel.X86_64Build {
		return r.before
	}
	return builds.FileCond{File: r.before.File, Status: builds.StatusUnlisted, Region: kcond.Const(true)}
}

// CountedArches are the arches of the file's lines that count in the
// change, as Arches are the file's.
func (r Result) CountedArches() builds.FileArches {
	if r.count != nil {
		return r.countArches
	}
	return r.Arches
}

// Counted is the condition in build of the file's lines that count in the
// change, as Cond gives the file's.
func (r Result) Counted(build string) builds.FileCond {
	if fc, ok := r.counts[cmp.Or(build, kernel.X86_64Build)]; ok {
		return fc
	}
	fc := r.Cond(build)
	if r.count != nil && fc.Status != builds.StatusUnlisted {
		fc.Region = r.count // what is known without a revision
	}
	return fc
}

// Options tune a Resolve, a Diff or a Search.
type Options struct {
	Workers  int               // revisions crawled at once; 4 when 0
	Progress func(crawled int) // called after each revision crawled, with the count so far
	// Build, one of kernel.ConfigBuilds, is the only build read, and the
	// architectures are left out; every build is read when it is "".
	Build string
	// Config is the .config Diff gives verdicts for, when not nil.
	Config kcond.Config
	// Warn is told what the caller should know, as a range without
	// commits; nil tells nothing.
	Warn func(msg string)
	// Visit, when not nil, is told of each file Resolve finds at a
	// revision, with each build read there and the file's condition in
	// it, while the build is at hand; it may be called from several
	// goroutines at once, for different files.
	Visit func(f *File, b builds.Build, fc builds.FileCond)
}

const defaultWorkers = 4

// initial is what is known of a file before any revision is read.
func initial(f *File) Result {
	r := Result{before: builds.FileCond{File: f.File, Region: f.Region}, Arches: builds.FileArches{File: f.File},
		count: f.Count, countArches: builds.FileArches{File: f.File}}
	if f.Header {
		r.before.Status = builds.StatusHeader
		if pa := kernel.PathArch(f.Path); pa != "" && pa != kernel.Primary {
			r.before.Status = builds.StatusNotBuilt
		}
	}
	return r
}

// Resolve fills in the Kbuild condition and the architectures of every
// file at the first of its revisions that has it, crawling each revision
// once for all the files that need it. A header is never missing: without
// a revision that has it, it keeps the status header. The results are in
// the order of files.
func Resolve(ctx context.Context, dir string, files []*File, opts Options) ([]Result, error) {
	workers := opts.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	results := make([]Result, len(files))
	next := make([]int, len(files))
	var pending []int
	for i, f := range files {
		results[i] = initial(f)
		if len(f.Revs) > 0 {
			pending = append(pending, i)
		} else if !f.Header {
			results[i].before.Status = builds.StatusMissing
		}
	}
	crawled := 0
	for len(pending) > 0 {
		byRev := map[string][]int{}
		for _, i := range pending {
			rev := files[i].Revs[next[i]]
			byRev[rev] = append(byRev[rev], i)
		}
		pending = pending[:0]
		revs := slices.Sorted(maps.Keys(byRev))
		// Revisions are crawled in parallel; each owns its files. The
		// first error ends the others.
		found := make([][]Result, len(revs))
		cctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, workers)
		for i, rev := range revs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if cctx.Err() != nil {
					return
				}
				in := make([]*File, len(byRev[rev]))
				for j, k := range byRev[rev] {
					in[j] = files[k]
				}
				res, err := Rev(cctx, dir, rev, in, opts)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					return
				}
				found[i] = res
				crawled++
				if opts.Progress != nil {
					opts.Progress(crawled)
				}
			}()
		}
		wg.Wait()
		cancel()
		if firstErr != nil {
			return nil, firstErr
		}
		for ri, rev := range revs {
			for fi, i := range byRev[rev] {
				if found[ri] != nil && found[ri][fi].Rev != "" {
					results[i] = found[ri][fi]
					continue
				}
				if next[i]++; next[i] < len(files[i].Revs) {
					pending = append(pending, i)
				} else if !files[i].Header {
					results[i].before.Status = builds.StatusMissing
				}
			}
		}
	}
	return results, nil
}

// Rev computes the conditions and architectures at rev of the files that
// exist there, in the order of files; a file rev lacks has an empty
// result. opts are as for Resolve, but for Workers and Progress. A revision the tree does not have, or
// without a top-level Makefile, has no files.
func Rev(ctx context.Context, dir, rev string, files []*File, opts Options) ([]Result, error) {
	tree, err := gitfs.Open(ctx, dir, rev)
	if err != nil {
		var gerr *gitfs.Error
		if errors.As(err, &gerr) {
			return nil, nil // e.g. the parent of a root commit
		}
		return nil, err
	}
	defer tree.Close()
	results := make([]Result, len(files))
	var found []int
	for i, f := range files {
		if _, err := fs.Stat(tree, f.Path); err == nil {
			results[i] = initial(f)
			results[i].Rev = rev
			found = append(found, i)
		}
	}
	if len(found) == 0 {
		return results, nil
	}
	// add records what build b says of the files, and with arches of
	// their architectures.
	add := func(b builds.Build, arches bool) error {
		for _, i := range found {
			f, r := files[i], &results[i]
			fc := b.Eval(f.File, f.Region)
			if opts.Visit != nil {
				opts.Visit(f, b, fc)
			}
			if arches {
				r.Arches.Add(b, fc)
			}
			if f.Count != nil {
				cc := b.Eval(f.File, f.Count)
				if r.counts == nil {
					r.counts = map[string]builds.FileCond{}
				}
				r.counts[b.Name] = cc
				if arches {
					r.countArches.Add(b, cc)
				}
			}
			if r.Conds == nil {
				r.Conds = map[string]builds.FileCond{}
			}
			r.Conds[b.Name] = fc
		}
		return ctx.Err()
	}
	var paths []string
	for _, i := range found {
		paths = append(paths, files[i].Path)
	}
	s := builds.NewSession(tree, paths)
	if opts.Build != "" {
		b, ok := s.Build(opts.Build)
		switch {
		case errors.Is(b.Err, kbuild.ErrNoMakefile):
			return nil, nil
		case !ok:
			return results, nil // the revision lacks the build
		case b.Kbuild == nil:
			return nil, b.Err // e.g. the #include scan failed
		}
		return results, add(b, false)
	}
	// The x86-64 build comes first; its crawl failing fails the revision.
	err = s.Builds(func(b builds.Build) error {
		if b.Kbuild == nil && (b.X86_64 || errors.Is(b.Err, kbuild.ErrNoMakefile)) {
			return b.Err // no top-level Makefile, or e.g. the #include scan failed
		}
		return add(b, true)
	})
	if errors.Is(err, kbuild.ErrNoMakefile) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return results, nil
}
