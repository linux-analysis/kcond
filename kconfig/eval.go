package kconfig

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Set is a set of tristate values.
type Set uint8

const (
	N Set = 1 << iota
	M
	Y
)

func (s Set) String() string {
	if s == 0 {
		return "-"
	}
	var b strings.Builder
	for i, c := range "nmy" {
		if s&(1<<i) != 0 {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// lift applies a binary tristate operation to every pair of values.
func lift(a, b Set, f func(x, y int) int) Set {
	var r Set
	for x := range 3 {
		if a&(1<<x) == 0 {
			continue
		}
		for y := range 3 {
			if b&(1<<y) != 0 {
				r |= 1 << f(x, y)
			}
		}
	}
	return r
}

func andSet(a, b Set) Set { return lift(a, b, func(x, y int) int { return min(x, y) }) }
func orSet(a, b Set) Set  { return lift(a, b, func(x, y int) int { return max(x, y) }) }

func notSet(a Set) Set {
	var r Set
	for x := range 3 {
		if a&(1<<x) != 0 {
			r |= 1 << (2 - x)
		}
	}
	return r
}

// promote turns m into y, as kconfig does for bool symbols.
func promote(s Set) Set {
	if s&M != 0 {
		return s&^M | Y
	}
	return s
}

// Options adjust the analysis.
type Options struct {
	// Off lists symbols taken as n whatever their definition, such as
	// COMPILE_TEST.
	Off []string
}

// Values holds the values each bool and tristate symbol of a Tree can
// take in some valid configuration. Symbols are treated as independent of
// each other, so the sets can be too wide but never too narrow, except for
// what macro calls and joint constraints hide.
type Values struct {
	selectsOnce sync.Once
	selects     map[string][]selection // by selecting symbol, see Selects
	requiresMu  sync.Mutex
	requires    map[string][]string // by bare name, see Requires
	// searched holds the answers of Theory.Possible's search, by the
	// text of the condition, see Theory.Possible.
	searched sync.Map

	t       *Tree
	sets    []Set // by Symbol.idx; a symbol not tracked, or off, is n
	modules Set   // the value of the modules symbol in the world being evaluated
}

// tracked tells whether the analysis gives s a set: a bool or tristate
// some Kconfig file defines.
func (s *Symbol) tracked() bool {
	return len(s.Files) > 0 && (s.Type == Bool || s.Type == Tristate)
}

// Analyze computes the value sets of t's symbols.
func Analyze(t *Tree, opts Options) *Values {
	v := &Values{t: t, sets: make([]Set, len(t.list))}
	off := map[string]bool{}
	for _, s := range opts.Off {
		off[s] = true
	}
	var syms []*Symbol
	for _, s := range t.list {
		switch {
		case !s.tracked():
		case off[s.Name]:
			v.sets[s.idx] = N
		default:
			syms = append(syms, s)
		}
	}
	slices.SortFunc(syms, func(a, b *Symbol) int { return strings.Compare(a.Name, b.Name) })
	for {
		for changed := true; changed; {
			changed = false
			for _, s := range syms {
				old := v.sets[s.idx]
				if n := old | v.derive(s); n != old {
					v.sets[s.idx] = n
					changed = true
				}
			}
		}
		// What is still unknown takes part only in dependency cycles,
		// which kconfig rejects; take n and go on.
		stuck := false
		for _, s := range syms {
			if v.sets[s.idx] == 0 {
				v.sets[s.idx] = N
				stuck = true
			}
		}
		if !stuck {
			break
		}
	}
	return v
}

// modulesSet is the value set of the modules symbol, n without one.
func (v *Values) modulesSet() Set {
	if s := v.lookupSym(v.t.modules); s != 0 {
		return s
	}
	return N | Y
}

// derive computes the values s can take from the current sets. A result
// of 0 means it cannot be told yet. Modules on and off are evaluated
// apart, because both a dependency on m and the demotion of tristates to
// bools follow the modules symbol.
func (v *Values) derive(s *Symbol) Set {
	if s.Transitional {
		if s.Type == Tristate {
			return N | M | Y
		}
		return N | Y
	}
	mods := v.modulesSet()
	var r Set
	for _, w := range []Set{N, Y} {
		if mods&w == 0 && !(w == Y && mods&M != 0) {
			continue
		}
		v.modules = w
		d := v.deriveIn(s)
		if d == 0 {
			v.modules = 0
			return 0
		}
		r |= d
	}
	v.modules = 0
	return r
}

// deriveIn derives s with the modules symbol fixed to v.modules.
func (v *Values) deriveIn(s *Symbol) Set {
	tri := s.Type == Tristate && v.modules == Y
	vis := v.visibility(s, tri)
	if vis == 0 {
		return 0
	}
	if s.Choice != nil {
		return v.choiceMember(s, vis, tri)
	}
	var r Set
	if vis&(M|Y) != 0 {
		user := N | Y
		if tri {
			user |= M
		}
		r |= andSet(user, vis&^N)
	}
	d := v.defaultValue(s, tri)
	rev := v.anyOf(s.RevDep, N)
	if d == 0 || rev == 0 {
		return 0
	}
	if !tri {
		rev = promote(rev)
	}
	r = orSet(r|d, rev)
	if !tri {
		r = promote(r)
	}
	return r
}

// visibility is the value set of the OR of s's prompt conditions.
func (v *Values) visibility(s *Symbol, tri bool) Set {
	vis := v.anyOf(s.Prompts, N)
	if !tri {
		vis = promote(vis)
	}
	return vis
}

// anyOf is the OR of the conditions, or none when there are none.
func (v *Values) anyOf(es []*Expr, none Set) Set {
	if len(es) == 0 {
		return none
	}
	r := Set(0)
	for i, e := range es {
		c := v.eval(e)
		if c == 0 {
			return 0
		}
		if i == 0 {
			r = c
		} else {
			r = orSet(r, c)
		}
	}
	return r
}

// defaultValue is the value s takes from its defaults and implies when the
// user sets nothing.
func (v *Values) defaultValue(s *Symbol, tri bool) Set {
	var d Set
	pending := true
	for _, df := range s.Defaults {
		c := v.eval(df.Cond)
		if c == 0 {
			return 0
		}
		if c&(M|Y) != 0 {
			val := v.eval(df.Value)
			if val == 0 {
				return 0
			}
			d |= andSet(val, c&^N)
		}
		if c&N == 0 {
			pending = false
			break
		}
	}
	if pending {
		d |= N
	}
	if len(s.Implied) == 0 {
		return d
	}
	imp := v.anyOf(s.Implied, N)
	dir := v.anyOf(s.DirDep, Y)
	if imp == 0 || dir == 0 {
		return 0
	}
	if !tri {
		imp, dir = promote(imp), promote(dir)
	}
	var r Set
	if imp&N != 0 {
		r |= d
	}
	if imp&^N != 0 {
		r |= andSet(orSet(d, imp&^N), dir)
	}
	return r
}

// choiceMember derives a choice member: one visible member is y, the
// others n; optional and tristate choices (before v6.11) allow all n, and
// tristate choices allow m.
func (v *Values) choiceMember(s *Symbol, vis Set, tri bool) Set {
	ch := s.Choice
	var r Set
	if vis&(M|Y) != 0 {
		r |= Y
		if ch.Tristate && tri {
			r |= M
		}
	}
	if vis&N != 0 || ch.Optional || ch.Tristate {
		r |= N
	}
	for _, o := range ch.Members {
		if o == s || r&N != 0 {
			continue
		}
		ov := v.visibility(o, o.Type == Tristate && v.modules == Y)
		if ov == 0 {
			return 0
		}
		if ov&(M|Y) != 0 {
			r |= N
		}
	}
	return r
}

// lookup is the current set of a symbol by name: n for symbols that are
// undefined, not bool or tristate, or off.
func (v *Values) lookup(name string) Set {
	return v.lookupSym(v.t.Syms[name])
}

// lookupSym is the current set of s, n for nil or a symbol not tracked.
func (v *Values) lookupSym(s *Symbol) Set {
	if s == nil || !s.tracked() {
		return N
	}
	return v.sets[s.idx]
}

// eval is the value set of a Kconfig expression; nil is y.
func (v *Values) eval(e *Expr) Set {
	if e == nil {
		return Y
	}
	switch e.Op {
	case OpSym:
		switch e.Name {
		case "y":
			return Y
		case "m":
			return M
		case "n":
			return N
		case modulesRef, v.t.Modules:
			if v.modules != 0 {
				return v.modules
			}
			return v.lookup(v.t.Modules)
		}
		if e.Quoted {
			return N
		}
		return v.lookupSym(e.sym)
	case OpNot:
		return notSet(v.eval(e.L))
	case OpAnd:
		return andSet(v.eval(e.L), v.eval(e.R))
	case OpOr:
		return orSet(v.eval(e.L), v.eval(e.R))
	case OpNo:
		x := v.eval(e.L)
		var r Set
		if x&N != 0 {
			r |= Y
		}
		if x&^N != 0 {
			r |= N
		}
		return r
	case OpOpaque:
		return N | Y
	}
	return v.compare(e)
}

// compare evaluates a comparison over the string values of its operands.
func (v *Values) compare(e *Expr) Set {
	l, lok := v.strings(e.L)
	r, rok := v.strings(e.R)
	if !lok || !rok {
		return N | Y
	}
	if len(l) == 0 || len(r) == 0 {
		return 0
	}
	var res Set
	for _, a := range l {
		for _, b := range r {
			c := compareValues(a, b)
			var t bool
			switch e.Op {
			case OpEq:
				t = c == 0
			case OpNeq:
				t = c != 0
			case OpLt:
				t = c < 0
			case OpLe:
				t = c <= 0
			case OpGt:
				t = c > 0
			case OpGe:
				t = c >= 0
			}
			if t {
				res |= Y
			} else {
				res |= N
			}
		}
	}
	return res
}

// compareValues compares numerically when both values are numbers.
func compareValues(a, b string) int {
	if x, ok := number(a); ok {
		if y, ok := number(b); ok {
			return cmp.Compare(x, y)
		}
	}
	return strings.Compare(a, b)
}

// number reads s as a decimal or 0x number. Only a digit or a sign starts
// one, which spares the usual y, m, n and names the error ParseInt
// allocates.
func number(s string) (int64, bool) {
	if s == "" || (s[0] < '0' || s[0] > '9') && s[0] != '-' && s[0] != '+' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 0, 64)
	return n, err == nil
}

// strings returns the string values an operand can have; ok is false when
// they are unknown (a macro, or a string or number symbol). An empty list
// means it cannot be told yet.
func (v *Values) strings(e *Expr) ([]string, bool) {
	if e.Op != OpSym {
		return nil, false
	}
	name := e.Name
	if name == modulesRef {
		name = v.t.Modules
	}
	s := v.t.Syms[name]
	switch {
	case e.Quoted || name == "y" || name == "m" || name == "n":
		return []string{e.Name}, true
	case s == nil || len(s.Files) == 0:
		return []string{name}, true // an undefined symbol's value is its name
	case s.Type == Bool || s.Type == Tristate:
		var out []string
		set := v.lookupSym(s)
		if name == v.t.Modules && v.modules != 0 {
			set = v.modules
		}
		for i, c := range []string{"n", "m", "y"} {
			if set&(1<<i) != 0 {
				out = append(out, c)
			}
		}
		return out, true
	case s.Env != "":
		if val, ok := v.t.Env[s.Env]; ok {
			return []string{val}, true
		}
	}
	return nil, false
}

// Of returns the values a symbol, named with or without CONFIG_, can take
// in a .config: n for undefined, non-tristate and transitional symbols.
func (v *Values) Of(name string) Set {
	name = strings.TrimPrefix(name, "CONFIG_")
	if s := v.t.Syms[name]; s != nil && s.Transitional {
		return N
	}
	return v.lookup(name)
}

// Modular tells whether a symbol, named with or without CONFIG_, can be m
// in some valid configuration; known is false when the tree defines no
// such bool or tristate symbol, so that nothing is known of it. The sets
// are never too narrow (see Values), so a symbol that cannot be m here
// never is.
func (v *Values) Modular(name string) (modular, known bool) {
	name = strings.TrimPrefix(name, "CONFIG_")
	s := v.t.Syms[name]
	if s == nil || !s.tracked() || s.Transitional {
		return false, false
	}
	return v.lookupSym(s)&M != 0, true
}

// Selects lists, sorted and with CONFIG_, the symbols that name (with or
// without CONFIG_) selects whenever it is enabled: they are y when it is y
// and at least m when it is m. A select under a condition the symbol's own
// dependencies do not settle is left out.
func (v *Values) Selects(name string) []string {
	name = strings.TrimPrefix(name, "CONFIG_")
	v.selectsOnce.Do(func() {
		v.selects = map[string][]selection{}
		for _, s := range v.t.Syms {
			if len(s.Files) == 0 {
				continue
			}
			for _, e := range s.RevDep {
				// e is the selecting symbol, ANDed with the condition of
				// its "if" and then with the dependencies of its
				// definition, which hold whenever it is enabled when that
				// is its only one.
				by := e
				for by.Op == OpAnd {
					by = by.L
				}
				if by.Op != OpSym || by.Quoted {
					continue
				}
				if sel := v.t.Syms[by.Name]; e.Op == OpAnd && sel != nil && len(sel.DirDep) == 1 && sel.DirDep[0] == e.R {
					e = e.L
				}
				if syms, ok := conjunctSyms(e); ok {
					v.selects[by.Name] = append(v.selects[by.Name], selection{s.Name, syms})
				}
			}
		}
	})
	var out []string
	required := v.Requires(name)
	for _, sel := range v.selects[name] {
		if !slices.ContainsFunc(sel.under, func(c string) bool { return c != name && !slices.Contains(required, "CONFIG_"+c) }) {
			out = append(out, "CONFIG_"+sel.sym)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// selection is a symbol some symbol selects, under the conjunction of
// symbols the select carries: the selecting symbol and the condition of
// its "if".
type selection struct {
	sym   string
	under []string
}

// conjunctSyms returns the symbols ANDed in e; ok is false when e holds
// anything else.
func conjunctSyms(e *Expr) (syms []string, ok bool) {
	switch {
	case e == nil:
		return nil, true
	case e.Op == OpSym && !e.Quoted:
		return []string{e.Name}, true
	case e.Op == OpAnd:
		l, ok := conjunctSyms(e.L)
		if !ok {
			return nil, false
		}
		r, ok := conjunctSyms(e.R)
		return append(l, r...), ok
	}
	return nil, false
}

// Tristate reports whether the tree defines name (with or without
// CONFIG_) as a tristate symbol.
func (v *Values) Tristate(name string) bool {
	s := v.t.Syms[strings.TrimPrefix(name, "CONFIG_")]
	return s != nil && len(s.Files) > 0 && s.Type == Tristate
}

// Defined reports whether some Kconfig file the tree read defines the
// symbol, named with or without CONFIG_, whatever its type.
func (v *Values) Defined(name string) bool {
	s := v.t.Syms[strings.TrimPrefix(name, "CONFIG_")]
	return s != nil && len(s.Files) > 0
}

// Requires returns the bool and tristate symbols, as CONFIG_ names, that
// the symbol name (with or without CONFIG_) needs enabled to be enabled
// itself: those every definition of it depends on, directly or through
// their own dependencies. Only plain symbols ANDed in a dependency count,
// including those the tree never defines, which keep it n.
// A select that ignores unmet dependencies is a Kconfig warning and does
// not count either. Each symbol's are computed once; Requires is safe for
// concurrent use.
func (v *Values) Requires(name string) []string {
	name = strings.TrimPrefix(name, "CONFIG_")
	v.requiresMu.Lock()
	defer v.requiresMu.Unlock()
	out, ok := v.requires[name]
	if !ok {
		out = configNames(v.requiresOf(name, map[string]map[string]bool{}))
		if v.requires == nil {
			v.requires = map[string][]string{}
		}
		v.requires[name] = out
	}
	return slices.Clone(out) // the caller may change it
}

// DependsOn returns the bool and tristate symbols, as CONFIG_ names, that
// every definition of the symbol name (with or without CONFIG_) depends on
// directly: Requires without the dependencies of those.
func (v *Values) DependsOn(name string) []string {
	s := v.t.Syms[strings.TrimPrefix(name, "CONFIG_")]
	if s == nil || s.Choice != nil {
		return nil
	}
	return configNames(v.everyDefinition(s, nil))
}

// requiresOf is Requires on bare names. memo holds the sets computed so
// far, and nil for the symbols being computed, where a dependency cycle
// stops.
func (v *Values) requiresOf(name string, memo map[string]map[string]bool) map[string]bool {
	if r, ok := memo[name]; ok {
		return r
	}
	memo[name] = nil
	s := v.t.Syms[name]
	if s == nil || s.Choice != nil {
		return nil
	}
	out := v.everyDefinition(s, func(d string, cur map[string]bool) {
		for r := range v.requiresOf(d, memo) {
			cur[r] = true
		}
	})
	memo[name] = out
	return out
}

// everyDefinition returns the symbols every definition of s gives: each
// one ANDed at the top of the definition's dependencies, with what expand,
// when not nil, adds for it; s itself is left out.
func (v *Values) everyDefinition(s *Symbol, expand func(d string, cur map[string]bool)) map[string]bool {
	var out map[string]bool
	for i, dep := range s.DirDep {
		cur := map[string]bool{}
		for _, d := range v.conjuncts(dep) {
			cur[d] = true
			if expand != nil {
				expand(d, cur)
			}
		}
		if i == 0 {
			out = cur
		} else {
			maps.DeleteFunc(out, func(k string, _ bool) bool { return !cur[k] })
		}
	}
	delete(out, s.Name)
	return out
}

// configNames returns the symbols of set as CONFIG_ names, sorted.
func configNames(set map[string]bool) []string {
	var out []string
	for d := range set {
		out = append(out, "CONFIG_"+d)
	}
	slices.Sort(out)
	return out
}

// conjuncts returns the bool and tristate symbols ANDed at the top of e,
// and those the tree never defines: always n, they are why a symbol that
// needs one is.
func (v *Values) conjuncts(e *Expr) []string {
	if e == nil {
		return nil
	}
	switch e.Op {
	case OpAnd:
		return append(v.conjuncts(e.L), v.conjuncts(e.R)...)
	case OpSym:
		switch e.Name {
		case "y", "m", "n", modulesRef:
			return nil
		}
		if s := v.t.Syms[e.Name]; !e.Quoted && (s == nil || len(s.Files) == 0 || s.Type == Bool || s.Type == Tristate) {
			return []string{e.Name}
		}
	}
	return nil
}
