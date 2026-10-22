package kbuild

import (
	"io/fs"
	"maps"
	"path"
	"slices"
	"sync"

	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/source"
)

// headerIndex is what the lookups of a crawl learn of its headers. A
// header is looked up when first asked about, in fsys; mu guards the
// lookups, so Lookup, Module and Define are safe for concurrent use.
// conds holds the condition of each header looked up, nil for one found
// unlisted, and parts its condition in C and in assembly units;
// includers holds the #include edges into the headers looked up, which
// the crawl's own edges into .c, .S and Rust files complete. roots finds
// the files <...> names in the build's include directories, and included
// lists by header the sources whose objects compile it through an
// -include flag, made at the first lookup with defined, the macros the -D
// flags of the objects define (see indexFlags).
type headerIndex struct {
	fsys      fs.FS
	mu        sync.Mutex
	conds     map[string]*kcond.Expr
	parts     map[string][2]*kcond.Expr
	includers map[string][]edge
	roots     *includeRoots
	included  map[string][]edge
	defined   map[string]bool
}

// includersOf returns the #include edges into f: a header's, once looked
// up, or a .c, .S or Rust file's, from the crawl.
func (r *Result) includersOf(f string) []edge {
	if in, ok := r.lazy.includers[f]; ok {
		return in
	}
	return r.includers[f]
}

// Define returns the conditions under which source p is compiled with the
// macro x a -D flag of its object defines, and without it. A file a source
// #includes is compiled with that source's flags.
func (r *Result) Define(p, x string) (set, unset *kcond.Expr) {
	r.lazy.mu.Lock()
	defer r.lazy.mu.Unlock()
	// No object defines it: a region's #ifdef of a macro the headers
	// define asks about every header, so the answer is read off.
	if r.indexFlags(); !r.lazy.defined[x] {
		e, st := r.lookup(p)
		if st != Built {
			e = kcond.Const(false)
		}
		return kcond.Const(false), e
	}
	nodes, e, many := r.reach(p)
	if many {
		return e, e
	}
	return r.fold(p, nodes, func(c compilation) (*kcond.Expr, *kcond.Expr) {
		var fs []*kcond.Expr
		for _, f := range c.flags {
			if f.kind == defineMacro && f.arg == x {
				fs = append(fs, f.cond)
			}
		}
		with := kcond.Or(fs...)
		return kcond.And(c.cond, with), kcond.And(c.cond, kcond.Not(with))
	})
}

// fold ORs the two conditions f gives each compilation of source p, and
// each of a file that #includes p, there under the directive's condition,
// through the includers of the includers, nodes (see reach): the
// conditions propagate up the include graph as directory conditions do,
// each file once, where a header of thousands of includers has too many
// paths to follow.
func (r *Result) fold(p string, nodes map[string]bool, f func(compilation) (a, b *kcond.Expr)) (a, b *kcond.Expr) {
	ra, rb := map[string]*kcond.Expr{}, map[string]*kcond.Expr{}
	edges := map[string][]edge{}
	for n := range nodes {
		edges[n] = r.includersOf(n)
		for _, c := range r.compiled[n] {
			ca, cb := f(c)
			ra[n], rb[n] = orExpr(ra[n], ca), orExpr(rb[n], cb)
		}
	}
	a, b = kcond.Const(false), kcond.Const(false)
	if c, ok := dirConditions(ra, edges, false)[p]; ok {
		a = c
	}
	if c, ok := dirConditions(rb, edges, false)[p]; ok {
		b = c
	}
	return a, b
}

// Module returns the conditions under which source p is compiled into the
// kernel and into a module: obj-m lists its object and obj-y does not, as
// Kbuild filters obj-m by obj-y, it is a member of such a composite, or
// such a source includes it, and the other way round. A file compiled both
// ways, as one source compiled on its own and #included into a module's,
// has both. Each is false where it never holds.
func (r *Result) Module(p string) (builtin, module *kcond.Expr) {
	r.lazy.mu.Lock()
	defer r.lazy.mu.Unlock()
	nodes, e, many := r.reach(p)
	if many {
		return e, kcond.And(e, kcond.Var("CONFIG_MODULES", kcond.Enabled))
	}
	return r.fold(p, nodes, func(c compilation) (*kcond.Expr, *kcond.Expr) { return c.builtin, c.module })
}

// maxFold bounds the sources whose compilations fold follows into a
// header.
const maxFold = 1000

// reach returns p and the files that include it, directly or through
// other files, a header looked up first, and for a header that more than
// maxFold compiled sources include, many and its condition.
// shortcut: the OR of the module or macro conditions of thousands of
// sources is as slow to build as it is long to read, so such a header
// reads as compiled either way wherever it is compiled, into a module
// where modules are built; refine if a change under #ifdef MODULE in a
// core header needs the exact condition.
func (r *Result) reach(p string) (nodes map[string]bool, e *kcond.Expr, many bool) {
	header := path.Ext(p) == ".h"
	st := Unlisted
	if header {
		e, st = r.lookup(p)
	}
	nodes = map[string]bool{p: true}
	for queue := []string{p}; len(queue) > 0; queue = queue[1:] {
		for _, in := range r.includersOf(queue[0]) {
			if !nodes[in.parent] {
				nodes[in.parent] = true
				queue = append(queue, in.parent)
			}
		}
	}
	if !header || st != Built {
		return nodes, nil, false
	}
	n := 0
	for f := range nodes {
		if r.compiled[f] != nil {
			n++
		}
	}
	return nodes, e, n > maxFold
}

// indexFlags indexes the objects' flags at the first header or macro
// lookup: by header, the sources compiling it through -include, every
// compiled source for the flags of LINUXINCLUDE; and the macros some -D
// flag defines.
func (r *Result) indexFlags() {
	if r.lazy.included != nil {
		return
	}
	r.lazy.included, r.lazy.defined = map[string][]edge{}, map[string]bool{}
	for src, cs := range r.compiled {
		if ext := path.Ext(src); ext == ".c" || ext == ".S" {
			for _, fl := range r.forced {
				r.lazy.included[fl.arg] = append(r.lazy.included[fl.arg], edge{src, fl.cond})
			}
		}
		for _, c := range cs {
			for _, f := range c.flags {
				switch {
				case f.kind == includedFile && path.Ext(f.arg) == ".h":
					r.lazy.included[f.arg] = append(r.lazy.included[f.arg], edge{src, f.cond})
				case f.kind == defineMacro:
					r.lazy.defined[f.arg] = true
				}
			}
		}
	}
}

// Lookup returns the condition of a source file or header. A header is
// built where the files that #include it are compiled, each under the
// condition of its directive's line, through other headers recursively:
// the OR over its includers, which the #include lines of the tree, listed
// at the first lookup, tell. A quoted name is found next to the including
// file, a bracketed one in the build's include directories (see
// includeRoots), either in the -I directories of the includer's object
// otherwise, and a header an -include flag names is included by the
// object's source, or by every one for the flags of LINUXINCLUDE. An
// includer that is not built, or that no Makefile lists, adds nothing, as
// for an included .c file. A header no listed file includes is unlisted.
func (r *Result) Lookup(p string) (*kcond.Expr, Status) {
	r.lazy.mu.Lock()
	defer r.lazy.mu.Unlock()
	return r.lookup(p)
}

// header looks a header up (see Lookup), memoizing the result in headers
// and includers. The includers are scanned a level at a time: the files
// including the header, then those including them. A header reached from
// no built source stays unlisted rather than not built. The C and the
// assembly units are kept apart, so that the __ASSEMBLY__ guards of the
// directives resolve.
func (r *Result) header(p string) (*kcond.Expr, Status) {
	if e, ok := r.lazy.conds[p]; ok {
		if e == nil {
			return nil, Unlisted
		}
		return e, Built
	}
	if r.lazy.fsys == nil {
		return nil, Unlisted
	}
	if r.lazy.roots == nil {
		r.lazy.roots = newIncludeRoots(r.lazy.fsys, r.Arch)
	}
	r.indexFlags()
	// roots are the conditions of the C and the assembly units reaching
	// p: the built sources, and the headers looked up before, whose
	// includers need no second scan.
	var roots [2]map[string]*kcond.Expr
	roots[0], roots[1] = map[string]*kcond.Expr{}, map[string]*kcond.Expr{}
	edges := map[string][]edge{}
	scanned := map[string]bool{p: true}
	// includer records that f includes h under cond: a header to scan
	// next, or a root.
	var next map[string]bool
	includer := func(h, f string, cond *kcond.Expr) {
		edges[h] = append(edges[h], edge{f, cond})
		if path.Ext(f) != ".h" {
			if e, st := r.lookup(f); st == Built {
				unit := 0
				if path.Ext(f) == ".S" {
					unit = 1
				}
				roots[unit][f] = e
			}
		} else if parts, ok := r.lazy.parts[f]; ok {
			roots[0][f], roots[1][f] = parts[0], parts[1]
		} else if _, done := r.lazy.conds[f]; !done && !scanned[f] {
			scanned[f] = true
			next[f] = true
		}
	}
	for level := map[string]bool{p: true}; len(level) > 0; {
		headers := slices.Sorted(maps.Keys(level))
		files, err := r.Includes.includers(r.lazy.fsys, headers)
		if err != nil {
			// shortcut: Lookup reports no error, so a read that fails
			// leaves the header unknown; report it if that misleads.
			r.lazy.conds[p] = nil
			return nil, Unlisted
		}
		next = map[string]bool{}
		bases := map[string]bool{}
		for _, h := range headers {
			bases[path.Base(h)] = true
		}
		for _, f := range slices.Sorted(maps.Keys(files)) {
			for _, d := range files[f] {
				if !bases[d.base] {
					continue
				}
				for _, t := range r.targets(f, d, level) {
					includer(t.file, f, t.cond)
				}
			}
		}
		for _, h := range headers {
			for _, e := range r.lazy.included[h] {
				includer(h, e.parent, e.cond)
			}
		}
		level = next
	}
	conds := map[string][2]*kcond.Expr{}
	for i, asm := range []bool{false, true} {
		es := map[string][]edge{}
		for h, in := range edges {
			for _, e := range in {
				es[h] = append(es[h], edge{e.parent, kcond.With(e.cond, source.AssemblyAtom, asm)})
			}
		}
		for h, c := range dirConditions(roots[i], es, false) {
			parts := conds[h]
			parts[i] = c
			conds[h] = parts
		}
	}
	for h := range scanned {
		if parts, ok := conds[h]; ok {
			for i, c := range parts {
				if c == nil {
					parts[i] = kcond.Const(false)
				}
			}
			// Module and Define fold over both units at once.
			in := make([]edge, len(edges[h]))
			for i, e := range edges[h] {
				in[i] = edge{e.parent, source.EitherUnit(e.cond)}
			}
			r.lazy.conds[h], r.lazy.parts[h], r.lazy.includers[h] = kcond.Or(parts[0], parts[1]), parts, in
		}
	}
	if c, ok := r.lazy.conds[p]; ok {
		return c, Built
	}
	r.lazy.conds[p] = nil
	return nil, Unlisted
}

// target is a file an #include names, under the condition of its line
// ANDed with that of the -I flag finding it.
type target struct {
	file string
	cond *kcond.Expr
}

// targets returns the headers of level that directive d of file f names.
func (r *Result) targets(f string, d incLine, level map[string]bool) []target {
	if d.quoted {
		if t, ok := resolveInclude(r.lazy.fsys, path.Dir(f), d.name); ok {
			if level[t] {
				return []target{{t, d.cond}}
			}
			return nil // another file of that name is nearer
		}
	}
	// The build's include directories come before an object's own -I
	// flags.
	if t, ok := r.lazy.roots.find(d.name); ok {
		if level[t] {
			return []target{{t, d.cond}}
		}
		return nil
	}
	// shortcut: a header has no object of its own, so the -I directories
	// of the objects including it are not searched for its directives.
	var out []target
	for _, t := range r.includeDirs(f, d.name, d.cond) {
		if level[t.file] {
			out = append(out, t)
		}
	}
	return out
}
