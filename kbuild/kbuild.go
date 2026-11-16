// Package kbuild computes Kbuild presence conditions: the CONFIG_ settings
// under which each kernel source file is compiled for an architecture. It
// crawls the tree from the top-level Makefile, evaluating each Makefile
// with variables whose values are words present under conditions, the way
// cvehound's crawler descends the tree but with conditional values,
// composite objects and directory conditions taken from the Makefile that
// lists each directory. A .c or .S file a listed source #includes is
// compiled with it, under that source's condition, and so is a header,
// where the files that #include it are (see Result.Lookup).
package kbuild

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/kernel"
)

// Options selects what to crawl.
type Options struct {
	// Arch is the source architecture under arch/, such as "x86". For x86,
	// trees before v2.6.24 use arch/x86_64.
	Arch string
	// Bits is the x86 word size Makefiles name objects by ($(BITS)): 64
	// unless set to 32, which crawls the i386 build.
	Bits string
	// Includes are the tree's #include directives that name .c and .S
	// files and its Rust module declarations, as ScanIncludes finds them;
	// nil scans the tree while the Makefiles are crawled, so fsys is read
	// from two goroutines. They are the same in every build, so the crawls
	// of one tree can share a scan.
	Includes *Includes
	// Lookups are the files the caller will look up (see Result.Lookup):
	// when one is a header, the tree's #include lines are listed during
	// the crawl rather than at the first lookup, for the crawls sharing
	// its Includes as well.
	Lookups []string
	// Regions are the conditions of header lines the caller will read
	// (see Result.Macros): the macros their defined() tests name are
	// grepped for during the crawl, all at once, for the crawls sharing its
	// Includes as well; another is grepped for when first read.
	Regions []*kcond.Expr
	// Dirs is what the directories of the tree listed in earlier crawls
	// of it (see Dirs); nil starts afresh.
	Dirs *Dirs
}

// Dirs holds what each directory's Makefile listed in the crawls of one
// tree so far, with what it read of the arch: the variables the build
// passes to every Makefile and those the top-level Makefile exports. A
// Makefile that reads the same of another build lists the same there, so
// that build takes the listing without evaluating it; a few Makefiles
// name $(SRCARCH) or append to the flags the arch Makefile sets, and are
// evaluated in each build. A Dirs is not safe for concurrent use.
type Dirs struct {
	done map[string]dirEntry // by Makefile path
}

type dirEntry struct {
	reads []outerRead
	dirResult
}

// reuse is what the Makefile mk listed in an earlier crawl, when it read
// the same of the arch as ev would.
func (ds *Dirs) reuse(mk string, ev *evaluator) (dirResult, bool) {
	en, ok := ds.done[mk]
	if !ok {
		return dirResult{}, false
	}
	for _, rd := range en.reads {
		if rd.exported {
			ws, ok := ev.topExport(rd.name)
			if ok != rd.ok || !slices.EqualFunc(ws, rd.words, sameWord) {
				return dirResult{}, false
			}
		} else if v, ok := ev.pre[rd.name]; ok != rd.ok || v != rd.value {
			return dirResult{}, false
		}
	}
	return en.dirResult, true
}

// sameWord tells whether two words are the same under the same condition,
// which is one node when it is the same (see kcond.Expr).
func sameWord(a, b cword) bool { return a.text == b.text && a.opaque == b.opaque && a.cond == b.cond }

// Status tells how a file relates to the build.
type Status uint8

const (
	// Built: a Makefile lists the file, or a listed source #includes it,
	// a header directly or through other headers; it is compiled when its
	// condition holds.
	Built Status = iota
	// Unlisted: no Makefile the crawl follows lists the file and no source
	// they list #includes it: the build enters its directory, as for a
	// file only a generated source includes, or the arch's own rules build
	// it, as for arch/arm/boot/compressed. A header is unlisted when no
	// listed file includes it (see Result.Lookup). Its condition is
	// unknown.
	Unlisted
	// NotBuilt: the file belongs to another architecture, is the twin of a
	// listed file for the other word size, or is outside arch/ in a
	// directory the build never enters.
	NotBuilt
	// NotSource: not a .c, .S, .rs or .h file.
	NotSource
)

func (s Status) String() string {
	return [...]string{"built", "unlisted", "not built", "not a source"}[s]
}

// Result holds the conditions of one tree.
type Result struct {
	Arch string // the arch/ directory used
	// Files maps each listed or #included source file to the condition
	// under which it is compiled.
	Files map[string]*kcond.Expr
	// Errors lists Makefile constructs that could not be evaluated.
	Errors []string
	// Includes are the #include directives the conditions of included
	// files were taken from, for other crawls of the tree to reuse.
	Includes *Includes
	// Dirs is what the directories listed, for other crawls of the tree
	// to reuse (see Dirs).
	Dirs *Dirs

	reached map[string]bool
	bits    string          // x86 only
	makes   map[string]bool // directories with a Makefile or Kbuild
	// compiled lists how each listed source is compiled, with its
	// object's flags; includers are the #include edges into each .c, .S
	// and Rust file, and incEdges the same with the macros each including
	// file defined before its directive. forced are the -include flags of
	// LINUXINCLUDE, which every object gets.
	compiled  map[string][]compilation
	includers map[string][]edge
	incEdges  map[string][]incEdge
	forced    []flag
	// subdirFlags holds each directory's subdir-ccflags-y and
	// subdir-asflags-y while the tree is crawled.
	subdirFlags map[string][2][]flag

	// The headers, which are looked up when first asked about (see
	// headerIndex); everything above is fixed once Crawl returns.
	lazy headerIndex
}

// flagKind tells what a compiler flag names.
type flagKind uint8

const (
	includeDir   flagKind = iota // -I or -iquote: a directory #include names are looked up in
	defineMacro                  // -D: a macro defined
	includedFile                 // -include: a file compiled ahead of the source
)

// flag is a -I directory, a -D macro or an -include file of an object's
// compiler flags, given under cond.
type flag struct {
	kind flagKind
	arg  string
	cond *kcond.Expr
}

// compilation is one way a source is compiled: under cond, into the
// kernel under builtin and into a module under module, with flags.
type compilation struct {
	cond, builtin, module *kcond.Expr
	flags                 []flag
}

func (r *Result) lookup(p string) (*kcond.Expr, Status) {
	if path.Ext(p) == ".h" {
		return r.header(p)
	}
	if !slices.Contains(sourceExts, path.Ext(p)) {
		return nil, NotSource
	}
	if e, ok := r.Files[p]; ok {
		return e, Built
	}
	if rest, ok := strings.CutPrefix(p, "arch/"); ok {
		// arch/x86/um is user-mode Linux on an x86 host, another arch.
		if a, _, _ := strings.Cut(rest, "/"); a != r.Arch || strings.HasPrefix(rest, "x86/um/") {
			return nil, NotBuilt
		}
	}
	// Makefiles name x86 objects by $(BITS); the twin of a listed file
	// for the other word size is not built.
	if r.bits != "" {
		other := map[string]string{"64": "_32.", "32": "_64."}[r.bits]
		if i := strings.LastIndex(p, other); i >= 0 {
			if _, ok := r.Files[p[:i]+"_"+r.bits+"."+p[i+4:]]; ok {
				return nil, NotBuilt
			}
		}
	}
	if r.reached[path.Dir(p)] {
		return nil, Unlisted
	}
	// A directory without a Makefile is part of the nearest one above it
	// that has one: its files are #included or built by that Makefile's
	// own rules.
	for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
		if r.makes[d] {
			if r.reached[d] {
				return nil, Unlisted
			}
			break
		}
	}
	// The arch's own rules, not obj-y, reach directories such as
	// arch/arm/boot/compressed.
	if strings.HasPrefix(p, "arch/") {
		return nil, Unlisted
	}
	return nil, NotBuilt
}

// includeDirs returns the files the -I directories of the objects compiled
// from src give the #include name on a line under cond.
func (r *Result) includeDirs(src, name string, cond *kcond.Expr) []target {
	var out []target
	for _, c := range r.compiled[src] {
		for _, f := range c.flags {
			if f.kind == includeDir {
				out = append(out, target{path.Join(f.arg, name), kcond.And(cond, f.cond)})
			}
		}
	}
	return out
}

// rootLists are the top-level variables that hold the directories the
// build starts from.
var rootLists = []string{"init-y", "core-y", "drivers-y", "net-y", "libs-y", "virt-y"}

// programs are the lists of host and user programs, named without .o.
var programs = []string{"hostprogs", "hostprogs-y", "hostprogs-always-y", "userprogs", "userprogs-always-y"}

// gate is the condition under which obj-y lists one of the objects of
// gate, false when it lists none.
func gate(ev *evaluator, objs []string) *kcond.Expr {
	ws := list(ev, "obj-y")
	var conds []*kcond.Expr
	for _, o := range objs {
		conds = append(conds, condOf(ws, o))
	}
	return kcond.Or(conds...)
}

// condOf returns the condition of the word text in ws, false when ws
// lacks it.
func condOf(ws []cword, text string) *kcond.Expr {
	for _, w := range ws {
		if w.text == text {
			return w.cond
		}
	}
	return kcond.Const(false)
}

// modes are the conditions under which an object is listed, and under
// which it is compiled into the kernel and into a module.
type modes struct{ cond, builtin, module *kcond.Expr }

// inKernel are the modes of an object of a list the kernel links.
func inKernel(cond *kcond.Expr) modes { return modes{cond, cond, kcond.Const(false)} }

func (m modes) and(c *kcond.Expr) modes {
	return modes{kcond.And(m.cond, c), kcond.And(m.builtin, c), kcond.And(m.module, c)}
}

// twins are the lists Kbuild reads together with their -m twins, which make
// keeps apart: obj-m beside obj-y, lib-m beside lib-y, drivers-m beside
// drivers-y in the top Makefile.
var twins = map[string]bool{"obj-y": true, "lib-y": true, "subdir-y": true, "always-y": true,
	"hostprogs-y": true, "hostprogs-always-y": true, "userprogs-always-y": true,
	"init-y": true, "core-y": true, "drivers-y": true, "net-y": true, "libs-y": true, "virt-y": true}

// list returns the words of a list, with those of its -m twin when Kbuild
// reads both.
func list(ev *evaluator, name string) []cword {
	ws := ev.value(name)
	if twins[name] {
		ws = merge(slices.Concat(ws, ev.value(strings.TrimSuffix(name, "-y")+"-m")))
	}
	return ws
}

// listing records that the Makefile of dir lists a source file, compiled
// with flags.
type listing struct {
	dir, src string
	modes
	flags []flag
	// linked is set for an object of obj-y or obj-m: Kbuild compiles its
	// built-in form only where the directory has need-builtin (see Crawl).
	linked bool
}

type edge struct {
	parent string
	cond   *kcond.Expr
}

// ErrNoMakefile is the error Crawl returns for a tree without a top-level
// Makefile.
var ErrNoMakefile = errors.New("kbuild: no top-level Makefile")

// Crawl computes the conditions of every source file of the tree in fsys.
func Crawl(fsys fs.FS, opts Options) (*Result, error) {
	if _, err := fs.Stat(fsys, "Makefile"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoMakefile, err)
	}
	arch := opts.Arch
	if _, err := fs.Stat(fsys, "arch/"+arch); err != nil && arch == "x86" {
		if _, err := fs.Stat(fsys, "arch/x86_64"); err == nil {
			arch = "x86_64"
		}
	}
	pre := map[string]string{"srctree": ".", "objtree": ".", "SRCARCH": arch, "ARCH": arch}
	r := &Result{Arch: arch, Files: map[string]*kcond.Expr{}, reached: map[string]bool{},
		makes: map[string]bool{}, compiled: map[string][]compilation{}, subdirFlags: map[string][2][]flag{},
		lazy: headerIndex{fsys: fsys, conds: map[string]*kcond.Expr{}, parts: map[string][2]*kcond.Expr{}, includers: map[string][]edge{}}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (d.Name() == "Makefile" || d.Name() == "Kbuild") {
			r.makes[path.Dir(p)] = true
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	var scan sync.WaitGroup
	var scanErr error
	if r.Includes = opts.Includes; r.Includes == nil {
		headers := slices.ContainsFunc(opts.Lookups, func(p string) bool { return path.Ext(p) == ".h" })
		var macros []string
		for _, e := range opts.Regions {
			for _, atom := range e.Opaque() {
				if x, ok := definedName(atom); ok {
					macros = append(macros, x)
				}
			}
		}
		scan.Go(func() { r.Includes, scanErr = scanIncludes(fsys, headers, macros) })
	}
	if r.Dirs = opts.Dirs; r.Dirs == nil {
		r.Dirs = &Dirs{done: map[string]dirEntry{}}
	}
	if arch == "x86" || arch == "x86_64" {
		r.bits = "64"
		if opts.Bits == "32" {
			r.bits = "32"
		}
		pre["BITS"] = r.bits
	}

	// The top-level Makefile, with the arch Makefile it includes, names
	// the directories the build starts from. Its conditions on make
	// variables we do not model (targets, O=, M=) are relaxed away: a
	// directory counts if any value of them lists it.
	top := newEvaluator(fsys, "", pre)
	top.run("Makefile")
	top.frozen = true
	r.Errors = append(r.Errors, top.errors...)
	// The words of a line share its condition; relaxing enumerates the
	// values of the atoms, so each condition is relaxed once.
	relaxed := map[*kcond.Expr]*kcond.Expr{}
	relax := func(e *kcond.Expr) *kcond.Expr {
		if _, ok := relaxed[e]; !ok {
			relaxed[e] = kcond.Relax(e)
		}
		return relaxed[e]
	}
	for _, f := range top.flags("LINUXINCLUDE") {
		if f.kind == includedFile && path.Ext(f.arg) == ".h" {
			r.forced = append(r.forced, flag{includedFile, f.arg, relax(f.cond)})
		}
	}
	roots := map[string]*kcond.Expr{}
	for _, name := range rootLists {
		for _, w := range list(top, name) {
			if d, ok := listedDir(w.text); ok && !w.opaque {
				roots[d] = orExpr(roots[d], relax(w.cond))
			}
		}
	}
	for _, d := range kernel.ArchDirs[arch] {
		if d = path.Join("arch", arch, d); r.makes[d] {
			roots[d] = kcond.Const(true)
		}
	}

	edges, linkEdges := map[string][]edge{}, map[string][]edge{}
	var listings []listing
	processed := map[string]bool{}
	var queue []string
	for d := range roots {
		queue = append(queue, d)
	}
	// Since v6.1 the root Kbuild lists the top directories, using lists
	// the top-level Makefile exports.
	if _, err := fs.Stat(fsys, "Kbuild"); err == nil {
		ev := newEvaluator(fsys, "", pre)
		ev.vars = top.vars
		ev.run("Kbuild")
		dr := collect(ev)
		listings = append(listings, r.listed("", dr)...)
		for _, s := range dr.subs {
			edges[s.dir] = append(edges[s.dir], edge{"", relax(s.cond)})
			linkEdges[s.dir] = append(linkEdges[s.dir], edge{"", relax(s.builtin)})
			queue = append(queue, s.dir)
		}
		processed[""] = true
	}
	slices.Sort(queue)
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if processed[d] {
			continue
		}
		processed[d] = true
		r.reached[d] = true
		mk := ""
		for _, name := range []string{"Kbuild", "Makefile"} {
			if _, err := fs.Stat(fsys, path.Join(d, name)); err == nil {
				mk = path.Join(d, name)
				break
			}
		}
		if mk == "" {
			continue
		}
		local := map[string]string{"src": d, "obj": d}
		for k, v := range pre {
			local[k] = v
		}
		ev := newEvaluator(fsys, d, local)
		ev.parent = top
		dr, ok := r.Dirs.reuse(mk, ev)
		if !ok {
			ev.run(mk)
			dr = collect(ev)
			r.Dirs.done[mk] = dirEntry{ev.reads, dr}
		}
		listings = append(listings, r.listed(d, dr)...)
		for _, s := range dr.subs {
			edges[s.dir] = append(edges[s.dir], edge{d, s.cond})
			linkEdges[s.dir] = append(linkEdges[s.dir], edge{d, s.builtin})
			r.reached[s.dir] = true
			if !processed[s.dir] {
				queue = append(queue, s.dir)
			}
		}
	}

	// The condition under which the build enters each directory, and the
	// one under which it links the directory's obj-y objects: the top
	// directories have need-builtin, and a directory passes it on to those
	// its obj-y lists (scripts/Makefile.build drops the directories of obj-y
	// without it). A directory entered through obj-m or subdir-y alone
	// compiles only its modules, lib-y and always-y.
	dirs := dirConditions(roots, edges, processed[""])
	links := dirConditions(roots, linkEdges, processed[""])
	memo, visiting := map[string][2][]flag{}, map[string]bool{}
	for _, l := range listings {
		dc, ok := dirs[l.dir]
		if l.dir == "" {
			dc, ok = kcond.Const(true), true
		}
		if !ok {
			continue
		}
		db := dc
		if l.linked && l.dir != "" {
			if db = links[l.dir]; db == nil {
				db = kcond.Const(false)
			}
		}
		// A linked object needs the directory linked, unless it is a module.
		c := kcond.And(dc, l.cond)
		if l.linked && db.String() != dc.String() {
			if l.module.IsConst(false) {
				c = kcond.And(db, l.cond)
			} else {
				c = kcond.And(c, kcond.Or(db, l.module))
			}
		}
		builtin, module := kcond.And(db, l.builtin), kcond.And(dc, l.module)
		r.Files[l.src] = orExpr(r.Files[l.src], c)
		fl := r.inherited(l.dir, edges, memo, visiting)
		sub := fl[0]
		if path.Ext(l.src) == ".S" {
			sub = fl[1]
		}
		r.compiled[l.src] = append(r.compiled[l.src], compilation{c, builtin, module, slices.Concat(l.flags, sub)})
	}
	if scan.Wait(); scanErr != nil {
		return nil, scanErr
	}
	r.include(fsys)
	r.subdirFlags = nil
	return r, nil
}

// inherited returns the subdir-ccflags-y and subdir-asflags-y flags that
// apply in directory d: its own and those of every directory the build
// enters it from.
func (r *Result) inherited(d string, edges map[string][]edge, memo map[string][2][]flag, visiting map[string]bool) [2][]flag {
	if fl, ok := memo[d]; ok || visiting[d] {
		return fl
	}
	visiting[d] = true
	fl := r.subdirFlags[d]
	for _, e := range edges[d] {
		p := r.inherited(e.parent, edges, memo, visiting)
		fl = [2][]flag{slices.Concat(fl[0], p[0]), slices.Concat(fl[1], p[1])}
	}
	delete(visiting, d)
	memo[d] = fl
	return fl
}

func orExpr(a, b *kcond.Expr) *kcond.Expr {
	if a == nil {
		return b
	}
	return kcond.Or(a, b)
}

// listedDir maps a top-level list entry (dir/, dir/built-in.o, dir/lib.a)
// to its directory.
func listedDir(w string) (string, bool) {
	for _, suf := range []string{"/built-in.o", "/built-in.a", "/lib.a", "/"} {
		if d, ok := strings.CutSuffix(w, suf); ok && d != "" {
			return cleanPath(d), true
		}
	}
	return "", false
}

// dirConditions propagates conditions from the roots down the edges, from
// a directory to those its Makefile lists or from a source to the files it
// #includes. A directory listed by several Makefiles gets the OR. A file is
// computed once its parents are, so that each is computed once; the graph
// can have cycles through ../, whose files are then iterated to a fixed
// point, a file changing 32 times stopping, which bounds a cycle.
func dirConditions(roots map[string]*kcond.Expr, edges map[string][]edge, rootKbuild bool) map[string]*kcond.Expr {
	conds := map[string]*kcond.Expr{}
	for d, c := range roots {
		conds[d] = c
	}
	if rootKbuild {
		conds[""] = kcond.Const(true)
	}
	files := slices.Sorted(maps.Keys(edges))
	children := map[string][]string{} // by parent
	pending := map[string]int{}       // parents still to compute
	for _, d := range files {
		for _, e := range edges[d] {
			children[e.parent] = append(children[e.parent], d)
			if edges[e.parent] != nil {
				pending[d]++
			}
		}
	}
	// compute reads d's condition off its parents' and reports a change.
	compute := func(d string) bool {
		var terms []*kcond.Expr
		if c, ok := roots[d]; ok {
			terms = append(terms, c)
		}
		for _, e := range edges[d] {
			if pc, ok := conds[e.parent]; ok {
				terms = append(terms, kcond.And(pc, e.cond))
			}
		}
		if len(terms) == 0 {
			return false
		}
		c := kcond.Or(terms...)
		if old, ok := conds[d]; ok && old == c {
			return false
		}
		conds[d] = c
		return true
	}
	var queue []string
	for _, d := range files {
		if pending[d] == 0 {
			queue = append(queue, d)
		}
	}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		compute(d)
		for _, ch := range children[d] {
			if pending[ch]--; pending[ch] == 0 {
				queue = append(queue, ch)
			}
		}
	}
	// The rest are in or below a cycle.
	queued := map[string]bool{}
	for _, d := range files {
		if pending[d] > 0 {
			queue = append(queue, d)
			queued[d] = true
		}
	}
	changes := map[string]int{}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		delete(queued, d)
		if !compute(d) {
			continue
		}
		if changes[d]++; changes[d] < 32 {
			for _, ch := range children[d] {
				if !queued[ch] {
					queued[ch] = true
					queue = append(queue, ch)
				}
			}
		}
	}
	delete(conds, "")
	return conds
}

type subdir struct {
	dir  string
	cond *kcond.Expr
	// builtin is the condition under which obj-y lists the directory,
	// which gives it need-builtin.
	builtin *kcond.Expr
}

// dirResult is what a directory's Makefile lists.
type dirResult struct {
	subs     []subdir
	listings []listing
	flags    [2][]flag // subdir-ccflags-y and subdir-asflags-y
	errors   []string
}

// listed records the errors and subdir flags of what directory d's
// Makefile lists, and returns its listings.
func (r *Result) listed(d string, dr dirResult) []listing {
	if dr.flags[0] != nil || dr.flags[1] != nil {
		r.subdirFlags[d] = dr.flags
	}
	r.Errors = append(r.Errors, dr.errors...)
	return dr.listings
}

// maxComposite bounds the nesting of composite objects.
const maxComposite = 10

// collect reads the objects and subdirectories a Makefile lists once it
// has been evaluated.
func collect(ev *evaluator) dirResult {
	ev.frozen = true
	var subs []subdir
	var ls []listing
	var words, objs []cword // objs: obj-y and obj-m, which link
	// An object is compiled into a module where obj-m lists it and obj-y
	// does not, as Kbuild filters obj-m by obj-y.
	ys, ms := ev.value("obj-y"), ev.value("obj-m")
	var own []string
	gates := map[string]*kcond.Expr{}
	for _, l := range kernel.OwnLists[ev.dir] {
		own = append(own, l.Name)
		if l.Gate != nil {
			gates[l.Name] = gate(ev, l.Gate)
		}
	}
	// extra-y and always-y name objects built beside the directory's
	// built-in.a, such as arch/x86/kernel's head code and vmlinux.lds.
	for _, name := range slices.Concat([]string{"obj-y", "lib-y", "subdir-y", "extra-y", "always-y"}, own, programs) {
		isOwn, isProg := slices.Contains(own, name), slices.Contains(programs, name)
		g, gated := gates[name]
		if gated && g.IsConst(false) {
			continue
		}
		for _, w := range list(ev, name) {
			if w.opaque {
				ev.note(w.text, name)
				continue
			}
			if name == "subdir-y" && !strings.HasSuffix(w.text, "/") {
				w.text += "/"
			}
			if isOwn {
				w.text = strings.TrimPrefix(w.text, ev.dir+"/")
			}
			if gated {
				w.cond = kcond.And(w.cond, g)
			}
			if isProg {
				w.text += ".o"
			}
			if name == "obj-y" {
				objs = append(objs, w)
			} else {
				words = append(words, w)
			}
		}
	}
	add := func(w cword, linked bool) {
		switch {
		case strings.HasSuffix(w.text, "/"):
			d := cleanPath(path.Join(ev.dir, w.text))
			if d != "." && !strings.HasPrefix(d, "../") {
				subs = append(subs, subdir{d, w.cond, condOf(ys, w.text)})
			}
		case strings.HasSuffix(w.text, ".o"):
			if d, ok := kernel.Wrappers[ev.dir][w.text]; ok {
				subs = append(subs, subdir{path.Join(ev.dir, d), w.cond, condOf(ys, w.text)})
			}
			k := inKernel(w.cond)
			if m := condOf(ms, w.text); !m.IsConst(false) {
				m = kcond.And(m, kcond.Not(condOf(ys, w.text)))
				k.builtin, k.module = kcond.And(w.cond, kcond.Not(m)), m
			}
			for _, l := range resolveObject(ev, w.text, k, map[string]bool{w.text: true}) {
				l.linked = linked
				ls = append(ls, l)
			}
		case strings.HasSuffix(w.text, ".lds"):
			ls = append(ls, leaf(ev, w.text, inKernel(w.cond))...) // X.lds from X.lds.S
		}
	}
	for _, w := range merge(words) {
		add(w, false)
	}
	for _, w := range merge(objs) {
		add(w, true)
	}
	// .s targets are compiled from C for the offsets headers they
	// generate, as the top-level Kbuild's kernel/bounds.s.
	for _, w := range ev.value("targets") {
		if stem, ok := strings.CutSuffix(w.text, ".s"); ok && !w.opaque {
			ls = append(ls, leaf(ev, stem, inKernel(w.cond))...)
		}
	}
	return dirResult{subs, ls, [2][]flag{ev.flags("subdir-ccflags-y"), ev.flags("subdir-asflags-y")}, ev.errors}
}

// objFlags returns the -I, -D and -include flags the Makefile gives the object obj
// compiled from a source with extension ext: those of the directory and
// those of the object, named as compiled (Kbuild's target-stem), which for
// a renamed object such as foo.stub.o may be foo.o.
func objFlags(ev *evaluator, obj, ext string) []flag {
	vars, prefix := []string{"ccflags-y", "EXTRA_CFLAGS"}, "CFLAGS_"
	switch ext {
	case ".c":
	case ".S":
		vars, prefix = []string{"asflags-y", "EXTRA_AFLAGS"}, "AFLAGS_"
	default:
		return nil
	}
	stem := strings.TrimSuffix(obj, ".o")
	for _, o := range []string{stem, strings.TrimSuffix(stem, path.Ext(stem))} {
		vars = append(vars, prefix+o+".o")
	}
	var fl []flag
	for _, v := range slices.Compact(vars) {
		fl = append(fl, ev.flags(v)...)
	}
	return fl
}

// resolveObject expands a composite object into its members, which share
// its modes, or maps a simple object to its source file. As Kbuild links
// them, the members of foo-objs and foo-y belong to the composite either
// way, those of foo-m only when it is a module.
func resolveObject(ev *evaluator, obj string, k modes, visited map[string]bool) []listing {
	stem := strings.TrimSuffix(obj, ".o")
	builtin, modular := merge(slices.Concat(ev.value(stem+"-objs"), ev.value(stem+"-y"))), ev.value(stem+"-m")
	members := merge(slices.Concat(builtin, modular))
	if len(members) > 0 && !(len(members) == 1 && members[0].text == obj) && len(visited) <= maxComposite {
		var out []listing
		for _, m := range members {
			if m.opaque {
				ev.note(m.text, stem+"-y")
				continue
			}
			if !strings.HasSuffix(m.text, ".o") {
				continue
			}
			c := k.and(condOf(builtin, m.text))
			if mod := kcond.And(k.module, condOf(modular, m.text)); !mod.IsConst(false) {
				c = modes{kcond.Or(c.cond, mod), c.builtin, kcond.Or(c.module, mod)}
			}
			if m.text == obj {
				out = append(out, leaf(ev, obj, c)...)
				continue
			}
			if visited[m.text] {
				continue
			}
			visited[m.text] = true
			out = append(out, resolveObject(ev, m.text, c, visited)...)
			delete(visited, m.text)
		}
		return out
	}
	return leaf(ev, obj, k)
}

// sourceExts are the extensions of the sources objects are compiled from,
// in the order a stem's source is looked for.
var sourceExts = []string{".c", ".S", ".rs", ".s"}

// maxObjectRules bounds the rules followed from one object to another.
const maxObjectRules = 4

// leaf maps an object to the source it is compiled from. A renamed object
// such as foo.stub.o or foo.pi.o, which a rule makes of foo.o, comes from
// foo's source when foo.stub itself has none. An object without a source
// of its own name comes from the one a rule of the Makefile names, as xe
// compiles i915's display code, or from the object the rule makes it of,
// as lkdtm objcopies rodata_objcopy.o of rodata.o; make would prefer the
// rule even where the object's own source exists, which no tree has. Make
// takes the first rule that applies, so one under a condition leaves the
// later ones where it does not hold.
func leaf(ev *evaluator, obj string, k modes) []listing { return leafAt(ev, obj, k, 0) }

// leafAt is leaf, depth rules away from the listed object.
func leafAt(ev *evaluator, obj string, k modes, depth int) []listing {
	stem := cleanPath(path.Join(ev.dir, strings.TrimSuffix(obj, ".o")))
	stems := slices.Compact([]string{stem, strings.TrimSuffix(stem, path.Ext(stem))})
	for _, s := range stems {
		for _, ext := range sourceExts {
			if _, err := fs.Stat(ev.fsys, s+ext); err == nil {
				return []listing{{ev.dir, s + ext, k, objFlags(ev, obj, ext), false}}
			}
		}
	}
	if !strings.HasSuffix(obj, ".o") {
		return nil
	}
	var out []listing
	taken := kcond.Const(false) // where an earlier rule applies
	for _, s := range stems {
		for _, ru := range ev.rules {
			if !matches(ru.target, s+".o") {
				continue
			}
			src := patsubst(ru.target, ru.source, s+".o")
			c := kcond.And(ru.cond, kcond.Not(taken))
			if c.IsConst(false) {
				continue
			}
			var more []listing
			if path.Ext(src) == ".o" {
				if depth < maxObjectRules {
					more = leafAt(ev, strings.TrimPrefix(src, ev.dir+"/"), k.and(c), depth+1)
				}
			} else if _, err := fs.Stat(ev.fsys, src); err == nil {
				more = []listing{{ev.dir, src, k.and(c), objFlags(ev, obj, path.Ext(src)), false}}
			}
			if len(more) == 0 {
				continue
			}
			out = append(out, more...)
			if taken = kcond.Or(taken, ru.cond); taken.IsConst(true) {
				return out
			}
		}
	}
	return out
}
