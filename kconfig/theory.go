package kconfig

import (
	"maps"
	"slices"

	"github.com/linux-analysis/kcond"
)

// Theory is what a build's Kconfig tells of conditions over its symbols:
// the values each can take, the symbols each requires and selects, and
// which symbols some Kconfig file of the tree defines, for any arch.
type Theory struct {
	Values *Values // nil when the Kconfig could not be read
	// Defined are the CONFIG_ symbols some Kconfig file of the whole tree
	// defines, which tells a symbol of another arch from one no Kconfig
	// has yet; nil when they could not be read.
	Defined map[string]bool
}

// maxBranches bounds the values Possible tries for the variables of a
// condition before it gives up.
const maxBranches = 1 << 12

// Possible tells whether e holds in some configuration of the build: True
// when values of its symbols, each in the set the Kconfig allows it (see
// Values.Of) and each enabled one with the symbols it requires enabled too
// (see Values.Requires), and values of its opaque atoms make it true,
// False when none do, and Unknown when maxBranches values tried could not
// tell. Symbols not in Defined are opaque (nil frees nothing), so one no
// Kconfig file defines yet, often added by the change itself, neither
// rules e in nor out.
func (th Theory) Possible(e *kcond.Expr) kcond.Tri {
	e = free(e, th.Defined)
	if g := th.guess(e); th.consistent(g) && value(e, g) == kcond.True {
		return kcond.True
	}
	// The search is what costs, and the conditions only it decides come
	// back: a shared header's, with each commit that changes it.
	var key string
	if th.Values != nil {
		key = e.String()
		if t, ok := th.Values.searched.Load(key); ok {
			return t.(kcond.Tri)
		}
	}
	syms := e.Symbols()
	dom := map[string]Set{}
	for _, names := range [][]string{syms, e.Opaque()} {
		for _, name := range names {
			dom[name] = th.domain(name)
		}
	}
	budget := maxBranches
	t := th.search(e, dom, th.requiredBy(syms), &budget)
	if th.Values != nil {
		th.Values.searched.Store(key, t)
	}
	return t
}

// Always tells whether e holds in every configuration of the build (see
// Possible).
func (th Theory) Always(e *kcond.Expr) kcond.Tri {
	return not(th.Possible(kcond.Not(e)))
}

// Implies tells whether code built under cond is built only where f
// holds: no configuration of the build makes cond true and f false (see
// Possible), so CONFIG_BONDING implies CONFIG_NET, and a bool CONFIG_X
// implies CONFIG_X=y.
func (th Theory) Implies(cond, f *kcond.Expr) kcond.Tri {
	return not(th.Possible(kcond.And(cond, kcond.Not(f))))
}

func not(t kcond.Tri) kcond.Tri {
	switch t {
	case kcond.True:
		return kcond.False
	case kcond.False:
		return kcond.True
	}
	return kcond.Unknown
}

// domain is the set of values a variable can take: a symbol's as the
// Kconfig allows, false (N) and true (Y) for an opaque atom.
func (th Theory) domain(name string) Set {
	switch {
	case !kcond.IsSymbol(name):
		return N | Y
	case th.Values == nil:
		return N | M | Y
	}
	return th.Values.Of(name)
}

func (th Theory) requires(name string) []string {
	if th.Values == nil || !kcond.IsSymbol(name) {
		return nil
	}
	return th.Values.Requires(name)
}

// prefer lists the values of v's variable that make the test v true, when
// want, or false, best first.
func prefer(v *kcond.Expr, want bool) []Set {
	switch {
	case v.State == kcond.Builtin && want:
		return []Set{Y}
	case v.State == kcond.Module && want:
		return []Set{M}
	case want:
		return []Set{Y, M}
	case v.State == kcond.Builtin:
		return []Set{N, M}
	case v.State == kcond.Module:
		return []Set{N, Y}
	}
	return []Set{N}
}

// guess gives each variable of e a value that tends to make e true: the
// one its first test in e asks for, within its domain, with the symbols an
// enabled one requires enabled.
func (th Theory) guess(e *kcond.Expr) map[string]Set {
	g := map[string]Set{}
	var walk func(e *kcond.Expr, want bool)
	walk = func(e *kcond.Expr, want bool) {
		switch e.Op {
		case kcond.OpVar:
			if _, ok := g[e.Name]; !ok {
				g[e.Name] = pick(th.domain(e.Name), prefer(e, want))
			}
		case kcond.OpNot:
			walk(e.Args[0], !want)
		default:
			for _, a := range e.Args {
				walk(a, want)
			}
		}
	}
	walk(e, true)
	for changed := true; changed; {
		changed = false
		for name, x := range g {
			for _, r := range th.requires(name) {
				if rx, ok := g[r]; x != N && ok && rx == N && th.domain(r)&^N != 0 {
					g[r], changed = pick(th.domain(r), []Set{Y, M}), true
				}
			}
		}
	}
	return g
}

// pick is the first of want in d, or else the first value of d.
func pick(d Set, want []Set) Set {
	for _, x := range append(want, Y, M, N) {
		if d&x != 0 {
			return x
		}
	}
	return N
}

// consistent tells whether every symbol g enables has the symbols it
// requires that g sets enabled too, and every value is in its domain.
func (th Theory) consistent(g map[string]Set) bool {
	for name, x := range g {
		if th.domain(name)&x == 0 {
			return false
		}
		for _, r := range th.requires(name) {
			if x != N && g[r] == N {
				return false
			}
		}
	}
	return true
}

// value is e's truth when each variable takes a value in its set in dom:
// True or False when all of them make it so, Unknown otherwise.
func value(e *kcond.Expr, dom map[string]Set) kcond.Tri {
	switch e.Op {
	case kcond.OpConst:
		if e.Val {
			return kcond.True
		}
		return kcond.False
	case kcond.OpVar:
		d, yes := dom[e.Name], Y|M
		switch e.State {
		case kcond.Builtin:
			yes = Y
		case kcond.Module:
			yes = M
		}
		switch {
		case d&^yes == 0:
			return kcond.True
		case d&yes == 0:
			return kcond.False
		}
		return kcond.Unknown
	case kcond.OpNot:
		return not(value(e.Args[0], dom))
	}
	short, res := kcond.False, kcond.True // And: False short-circuits
	if e.Op == kcond.OpOr {
		short, res = kcond.True, kcond.False
	}
	for _, a := range e.Args {
		switch value(a, dom) {
		case short:
			return short
		case kcond.Unknown:
			res = kcond.Unknown
		}
	}
	return res
}

// next is the first variable test of e that dom leaves open, reached
// through the first open operand of each AND and OR; e must be open. want
// is the value of the test that makes e true, flipped by each NOT on the
// way.
func next(e *kcond.Expr, dom map[string]Set) (v *kcond.Expr, want bool) {
	want = true
	for e.Op != kcond.OpVar {
		if e.Op == kcond.OpNot {
			e, want = e.Args[0], !want
			continue
		}
		for _, a := range e.Args {
			if value(a, dom) == kcond.Unknown {
				e = a
				break
			}
		}
	}
	return e, want
}

// search tells whether some values of the variables of e within dom, the
// domains narrowed so far, make e true, branching on its next open
// variable and trying first the values that make e true there, as guess
// does (see Possible); requiredBy lists the symbols of e that require
// each symbol.
func (th Theory) search(e *kcond.Expr, dom map[string]Set, requiredBy map[string][]string, budget *int) kcond.Tri {
	if t := value(e, dom); t != kcond.Unknown {
		return t
	}
	if *budget <= 0 {
		return kcond.Unknown
	}
	*budget--
	v, want := next(e, dom)
	d := dom[v.Name]
	res := kcond.False
	for _, x := range append(prefer(v, want), prefer(v, !want)...) {
		if d&x == 0 {
			continue
		}
		d &^= x
		narrowed, ok := th.fix(dom, requiredBy, v.Name, x)
		if !ok {
			continue
		}
		switch th.search(e, narrowed, requiredBy, budget) {
		case kcond.True:
			return kcond.True
		case kcond.Unknown:
			res = kcond.Unknown
		}
	}
	return res
}

// requiredBy lists, for each symbol, the symbols of syms that require it.
func (th Theory) requiredBy(syms []string) map[string][]string {
	by := map[string][]string{}
	for _, s := range syms {
		for _, r := range th.requires(s) {
			if r != s {
				by[r] = append(by[r], s)
			}
		}
	}
	return by
}

// fix sets the variable name to x in dom: the domains narrowed by what x
// asks of the others, an enabled symbol its requirements enabled and a
// disabled one the symbols that require it disabled (see requiredBy); ok
// is false when a domain is left empty.
func (th Theory) fix(dom map[string]Set, requiredBy map[string][]string, name string, x Set) (next map[string]Set, ok bool) {
	next = maps.Clone(dom)
	next[name] = x
	if x != N {
		for _, r := range th.requires(name) {
			d := th.domain(r)
			if nd, ok := next[r]; ok {
				d &= nd
			}
			if d &^= N; d == 0 {
				return nil, false
			}
			next[r] = d
		}
		return next, true
	}
	for _, s := range requiredBy[name] {
		if next[s]&N == 0 {
			return nil, false
		}
		next[s] = N
	}
	return next, true
}

// Plain writes the tests of e on symbols that cannot be m in the build
// plainly: X=y is X there, and X=m false. Kbuild conditions name both
// values of an option, obj-$(CONFIG_X) giving obj-y and obj-m, and the
// crawler cannot tell a bool from a tristate. A symbol the build's
// Kconfig lacks is false when another Kconfig of the tree defines it, as
// $(CONFIG_X) expands to nothing and #ifdef CONFIG_X fails there; one no
// Kconfig defines stays, as do tristates and every symbol when the
// Kconfig could not be read.
func (th Theory) Plain(e *kcond.Expr) *kcond.Expr {
	if th.Values == nil || e == nil {
		return e
	}
	// The gates are read before the rewrite, which drops the =m tests
	// of symbols this build cannot make modules, and after it, which
	// uncovers some.
	return th.shorten(th.gates(kcond.Rewrite(th.gates(e), func(v *kcond.Expr) *kcond.Expr {
		if kcond.IsSymbol(v.Name) && !th.Values.Defined(v.Name) && th.Defined[v.Name] {
			return kcond.Const(false)
		}
		if !kcond.IsSymbol(v.Name) || v.State == kcond.Enabled {
			return nil
		}
		if modular, known := th.Values.Modular(v.Name); !known || modular {
			return nil
		}
		if v.State == kcond.Module {
			return kcond.Const(false)
		}
		return kcond.Var(v.Name, kcond.Enabled)
	})))
}

// shorten drops from an OR a term that holds only with another term of
// the OR: Kconfig lets an option be on only with the options it depends on
// or selects, so CONFIG_JFFS2_FS || CONFIG_MTD is CONFIG_MTD, and so is
// (CONFIG_JFFS2_FS && CONFIG_X) || CONFIG_MTD or CONFIG_JFFS2_FS=m ||
// CONFIG_MTD. A tested value, CONFIG_MTD=y, is not what the option implies
// and absorbs nothing; a dropped term no longer absorbs, so that two
// options requiring each other keep one.
func (th Theory) shorten(e *kcond.Expr) *kcond.Expr {
	if e == nil || e.Op == kcond.OpVar || e.Op == kcond.OpConst {
		return e
	}
	args := make([]*kcond.Expr, 0, len(e.Args))
	for _, a := range e.Args {
		args = append(args, th.shorten(a))
	}
	if e.Op != kcond.OpOr {
		return rebuild(e, args)
	}
	plain := map[string]bool{} // the options the OR tests enabled
	for _, a := range args {
		if a.Op == kcond.OpVar && a.State == kcond.Enabled {
			plain[a.Name] = true
		}
	}
	implied := func(t *kcond.Expr) bool {
		vars := []*kcond.Expr{t}
		if t.Op == kcond.OpAnd {
			vars = t.Args
		}
		for _, v := range vars {
			if v.Op != kcond.OpVar || !kcond.IsSymbol(v.Name) {
				continue
			}
			if on := append(th.Values.Requires(v.Name), th.Values.Selects(v.Name)...); slices.ContainsFunc(on, func(n string) bool { return plain[n] }) {
				return true
			}
		}
		return false
	}
	out := make([]*kcond.Expr, 0, len(args))
	for _, t := range args {
		if implied(t) {
			delete(plain, t.Name)
			continue
		}
		out = append(out, t)
	}
	return rebuild(e, out)
}

// rebuild is e with args for its arguments, e itself when they are its.
func rebuild(e *kcond.Expr, args []*kcond.Expr) *kcond.Expr {
	if slices.Equal(args, e.Args) {
		return e
	}
	switch e.Op {
	case kcond.OpAnd:
		return kcond.And(args...)
	case kcond.OpOr:
		return kcond.Or(args...)
	}
	return kcond.Not(args[0])
}

// gates drops from an AND the alternative X=y || Y=m that a term on Y of
// the AND settles: Kconfig lets a tristate Y be y only when the options it
// depends on are y, so Y=y makes the X y, and Y=m holds the alternative. A
// Kbuild condition carries such a gate for an object listed by
// obj-$(CONFIG_Y) in a directory entered through obj-$(CONFIG_X), which
// links its built-in objects only with X=y.
func (th Theory) gates(e *kcond.Expr) *kcond.Expr {
	if e == nil || e.Op == kcond.OpVar || e.Op == kcond.OpConst {
		return e
	}
	names := map[string]bool{}
	if e.Op == kcond.OpAnd {
		for _, a := range e.Args {
			if a.Op == kcond.OpVar {
				names[a.Name] = true
			}
		}
	}
	args := make([]*kcond.Expr, 0, len(e.Args))
	for _, a := range e.Args {
		if !(a.Op == kcond.OpOr && th.gateSettled(a, names)) {
			args = append(args, th.gates(a))
		}
	}
	return rebuild(e, args)
}

// gateSettled tells whether or has a member Y=m, Y a tristate among names,
// whose other members hold once Y is y, with the options that are y then:
// what a y option selects, what a tristate at y depends on, and the bools
// a bool at y requires, since a bool is y wherever it is on but may be y
// on a tristate dependency at m. They are checked by evaluation, which
// proves them only when they have no negation.
func (th Theory) gateSettled(or *kcond.Expr, names map[string]bool) bool {
	for i, m := range or.Args {
		if m.Op != kcond.OpVar || m.State != kcond.Module || !names[m.Name] || !th.Values.Tristate(m.Name) {
			continue
		}
		cfg := kcond.Config{}
		for todo := []string{m.Name}; len(todo) > 0; todo = todo[1:] {
			if n := todo[0]; cfg[n] == 0 {
				cfg[n] = 'y'
				if th.Values.Tristate(n) {
					todo = append(todo, th.Values.DependsOn(n)...)
				} else {
					for _, r := range th.Values.Requires(n) {
						if !th.Values.Tristate(r) {
							todo = append(todo, r)
						}
					}
				}
				todo = append(todo, th.Values.Selects(n)...)
			}
		}
		rest := kcond.Or(slices.Delete(slices.Clone(or.Args), i, i+1)...)
		if !negated(rest) && rest.Eval(cfg) == kcond.True {
			return true
		}
	}
	return false
}

// negated reports whether e has a negation.
func negated(e *kcond.Expr) bool {
	return e.Op == kcond.OpNot || slices.ContainsFunc(e.Args, negated)
}

// free makes the CONFIG_ symbols of e that defined lacks opaque; nil
// defined frees nothing.
func free(e *kcond.Expr, defined map[string]bool) *kcond.Expr {
	if defined == nil {
		return e
	}
	switch e.Op {
	case kcond.OpVar:
		if kcond.IsSymbol(e.Name) && !defined[e.Name] {
			return kcond.Var("undefined:"+e.Name, kcond.Enabled)
		}
	case kcond.OpNot:
		return kcond.Not(free(e.Args[0], defined))
	case kcond.OpAnd, kcond.OpOr:
		args := make([]*kcond.Expr, len(e.Args))
		for i, a := range e.Args {
			args[i] = free(a, defined)
		}
		if e.Op == kcond.OpAnd {
			return kcond.And(args...)
		}
		return kcond.Or(args...)
	}
	return e
}
