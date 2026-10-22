package kconfig

import (
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

// Possible reports whether e holds in some configuration of the build.
// Symbols not in Defined are opaque (nil frees nothing), so one no Kconfig
// file defines yet, often added by the change itself, neither rules e in
// nor out.
func (th Theory) Possible(e *kcond.Expr) bool {
	return th.Values.Possible(free(e, th.Defined))
}

// Always reports whether e holds in every configuration of the build,
// symbols as for Possible.
func (th Theory) Always(e *kcond.Expr) bool {
	return th.Values.Always(free(e, th.Defined))
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
