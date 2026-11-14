package kbuild

import (
	"strings"

	"github.com/linux-analysis/kcond"
)

// incEdge is an #include edge into a .c, .S or Rust file, with the macros
// the including file has defined before the directive (see Include.Defs).
type incEdge struct {
	edge
	defs map[string]*kcond.Expr
}

// maxResolve bounds the includes resolve follows up from a file.
const maxResolve = 32

// Macros returns e, the condition of lines of source p, read with the
// macros the files including p #define before their directives: the OR,
// over the ways the build compiles p, of the condition of that way and e
// there (see resolve). It is e where no file above p defines a macro e
// tests, as for a header.
// shortcut: a header's includers are not read for the macros they define;
// refine if the region of a template header such as paging_tmpl.h matters.
func (r *Result) Macros(p string, e *kcond.Expr) *kcond.Expr {
	r.lazy.mu.Lock()
	defer r.lazy.mu.Unlock()
	if !r.definesAbove(p, e) {
		return e
	}
	return r.resolve(p, e, r.fileCond, map[string]bool{})
}

// resolve returns e, a condition of lines of file n, read in each way the
// build compiles n: on its own, under its compilations' conditions with e
// as it is, and wherever a file including it is, under the directive's
// condition with the macros that file defined before it (see define),
// further up as long as e tests macros defined there. cond gives a file's
// condition where nothing above it defines them, and for a file the walk
// comes back to, as qspinlock.c including itself, whose definitions would
// otherwise be read into e again at each turn; active holds the files on
// the way up.
func (r *Result) resolve(n string, e *kcond.Expr, cond func(string) *kcond.Expr, active map[string]bool) *kcond.Expr {
	if active[n] || len(active) >= maxResolve || !r.definesAbove(n, e) {
		return kcond.And(cond(n), e)
	}
	active[n] = true
	defer delete(active, n)
	return r.ways(n, e, cond, active)
}

// ways is the OR of e under each way n is compiled: under its
// compilations' conditions, and up each #include of it (see resolve).
func (r *Result) ways(n string, e *kcond.Expr, cond func(string) *kcond.Expr, active map[string]bool) *kcond.Expr {
	var terms []*kcond.Expr
	for _, c := range r.compiled[n] {
		terms = append(terms, kcond.And(c.cond, e))
	}
	for _, in := range r.incEdges[n] {
		terms = append(terms, r.resolve(in.parent, kcond.And(in.cond, define(e, in.defs)), cond, active))
	}
	return kcond.Or(terms...)
}

// resolveFiles reads again, by the ways it is compiled (see resolve), the
// condition of each file an #include under a test of a macro defined above
// it reaches, and of the files below it; the others keep theirs.
func (r *Result) resolveFiles() {
	var queue []string
	children := map[string][]string{}
	for t, ins := range r.incEdges {
		for _, in := range ins {
			children[in.parent] = append(children[in.parent], t)
			if r.definesAbove(in.parent, in.cond) {
				queue = append(queue, t)
			}
		}
	}
	below := map[string]bool{}
	for ; len(queue) > 0; queue = queue[1:] {
		if f := queue[0]; !below[f] {
			below[f] = true
			queue = append(queue, children[f]...)
		}
	}
	resolved := map[string]*kcond.Expr{}
	var cond func(string) *kcond.Expr
	cond = func(f string) *kcond.Expr {
		if !below[f] {
			return r.fileCond(f)
		}
		if c, ok := resolved[f]; ok {
			return c
		}
		resolved[f] = r.fileCond(f) // what an include cycle reads
		resolved[f] = r.ways(f, kcond.Const(true), cond, map[string]bool{})
		return resolved[f]
	}
	for f := range below {
		if _, ok := r.Files[f]; ok { // a file no listed source reaches stays out
			r.Files[f] = cond(f)
		}
	}
}

// fileCond is the condition of a source or included file, false for one
// the build never compiles.
func (r *Result) fileCond(f string) *kcond.Expr {
	if c, ok := r.Files[f]; ok {
		return c
	}
	return kcond.Const(false)
}

// definesAbove reports whether e tests a macro some file above p defines
// before including the next file down (see above).
func (r *Result) definesAbove(p string, e *kcond.Expr) bool {
	above := r.above(p)
	if len(above) == 0 {
		return false
	}
	for _, atom := range e.Opaque() {
		if name, ok := definedName(atom); ok && above[name] {
			return true
		}
	}
	return false
}

// above returns the macros the files including p, directly or through
// others, have defined or undefined before their directives. A file in an
// include cycle may miss some, which leaves its conditions as they were.
func (r *Result) above(p string) map[string]bool {
	if r.lazy.above == nil {
		r.lazy.above = map[string]map[string]bool{}
	}
	if m, ok := r.lazy.above[p]; ok {
		return m
	}
	r.lazy.above[p] = nil
	m := map[string]bool{}
	for _, in := range r.incEdges[p] {
		for x := range in.defs {
			m[x] = true
		}
		for x := range r.above(in.parent) {
			m[x] = true
		}
	}
	r.lazy.above[p] = m
	return m
}

// define is e, the condition of a line of a file another includes, read
// where that file has defined the macros of defs: defined(X) holds where
// defs says, or where it held, as a header the included file reads may
// define X too.
func define(e *kcond.Expr, defs map[string]*kcond.Expr) *kcond.Expr {
	if len(defs) == 0 {
		return e
	}
	return kcond.Subst(e, func(atom string) *kcond.Expr {
		if name, ok := definedName(atom); ok {
			if d, ok := defs[name]; ok {
				return kcond.Or(d, kcond.Var(atom, kcond.Enabled))
			}
		}
		return nil
	})
}

// definedName returns X for the atom defined(X).
func definedName(atom string) (string, bool) {
	name, ok := strings.CutPrefix(atom, "defined(")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(name, ")")
}
