// Package kcond represents and evaluates the conditions under which kernel
// code is built: Kbuild presence conditions and C preprocessor regions over
// Kconfig symbols.
package kcond

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Op is the kind of an Expr node.
type Op uint8

const (
	OpConst Op = iota
	OpVar
	OpNot
	OpAnd
	OpOr
)

// State is the tristate value a variable tests for.
type State uint8

const (
	Enabled State = iota // =y or =m
	Builtin              // =y
	Module               // =m
)

// Expr is an immutable boolean condition. Variables named CONFIG_* are
// Kconfig symbols; any other variable is an opaque atom (a make function, a
// non-Kconfig macro, a comparison) whose value is unknown. The constructors
// keep expressions normalized, so equal String forms mean equal trees.
type Expr struct {
	Op    Op
	Val   bool    // OpConst
	Name  string  // OpVar
	State State   // OpVar on CONFIG_ symbols
	Args  []*Expr // OpNot, OpAnd, OpOr
	key   string
}

var (
	trueExpr  = &Expr{Op: OpConst, Val: true, key: "true"}
	falseExpr = &Expr{Op: OpConst, Val: false, key: "false"}
)

// Const returns the constant v.
func Const(v bool) *Expr {
	if v {
		return trueExpr
	}
	return falseExpr
}

// IsConst reports whether e is the constant v.
func (e *Expr) IsConst(v bool) bool { return e.Op == OpConst && e.Val == v }

// Var returns a variable. st is ignored for opaque atoms.
func Var(name string, st State) *Expr {
	if !IsSymbol(name) {
		st = Enabled
	}
	e := &Expr{Op: OpVar, Name: name, State: st}
	e.key = e.render()
	return e
}

// IsSymbol reports whether name is a Kconfig symbol rather than an opaque
// atom.
func IsSymbol(name string) bool {
	return len(name) > len("CONFIG_") && strings.HasPrefix(name, "CONFIG_") && isIdent(name)
}

// Not returns the negation of e.
func Not(e *Expr) *Expr {
	switch e.Op {
	case OpConst:
		return Const(!e.Val)
	case OpNot:
		return e.Args[0]
	}
	n := &Expr{Op: OpNot, Args: []*Expr{e}}
	n.key = n.render()
	return n
}

// And returns the conjunction of es.
func And(es ...*Expr) *Expr { return nary(OpAnd, es) }

// Or returns the disjunction of es.
func Or(es ...*Expr) *Expr { return nary(OpOr, es) }

// nary flattens, folds constants, removes duplicates and complements, and
// applies absorption (A || A && B == A).
func nary(op Op, es []*Expr) *Expr {
	identity, absorbing := op == OpAnd, op != OpAnd
	var args []*Expr
	seen := map[string]bool{}
	var add func(e *Expr) bool
	add = func(e *Expr) bool {
		switch {
		case e.IsConst(identity):
			return true
		case e.Op == OpConst:
			return false
		case e.Op == op:
			for _, a := range e.Args {
				if !add(a) {
					return false
				}
			}
			return true
		}
		if !seen[e.key] {
			seen[e.key] = true
			args = append(args, e)
		}
		return true
	}
	for _, e := range es {
		if !add(e) {
			return Const(absorbing)
		}
	}
	for _, a := range args {
		if seen[Not(a).key] {
			return Const(absorbing)
		}
	}
	args, short := tristate(op, args)
	if short {
		return Const(absorbing)
	}
	args = absorb(op, args)
	if op == OpOr {
		if e := hoist(args); e != nil {
			return e
		}
	}
	switch len(args) {
	case 0:
		return Const(identity)
	case 1:
		return args[0]
	}
	// Order by symbol name first, so !A sorts next to A.
	slices.SortFunc(args, func(a, b *Expr) int {
		ka, kb := strings.TrimLeft(a.key, "(!"), strings.TrimLeft(b.key, "(!")
		if c := strings.Compare(ka, kb); c != 0 {
			return c
		}
		return strings.Compare(a.key, b.key)
	})
	n := &Expr{Op: op, Args: args}
	n.key = n.render()
	return n
}

// tristate merges the states one symbol is tested for: in an Or, X=y and
// X=m make X, and X covers both; in an And, X=y with X=m (or with !X) is
// false, X=y or X=m makes X redundant, and !X=y with !X=m makes !X. short
// reports that the result is the absorbing constant.
func tristate(op Op, args []*Expr) (out []*Expr, short bool) {
	pos := map[string]map[State]bool{}
	neg := map[string]map[State]bool{}
	for _, a := range args {
		v, m := a, pos
		if a.Op == OpNot {
			v, m = a.Args[0], neg
		}
		if v.Op == OpVar && IsSymbol(v.Name) {
			if m[v.Name] == nil {
				m[v.Name] = map[State]bool{}
			}
			m[v.Name][v.State] = true
		}
	}
	drop := map[string]bool{}
	var add []*Expr
	for name, st := range pos {
		switch {
		case op == OpOr && st[Enabled]:
			drop[Var(name, Builtin).key], drop[Var(name, Module).key] = true, true
		case op == OpOr && st[Builtin] && st[Module]:
			drop[Var(name, Builtin).key], drop[Var(name, Module).key] = true, true
			add = append(add, Var(name, Enabled))
		case op == OpAnd && st[Builtin] && st[Module]:
			return nil, true
		case op == OpAnd && (st[Builtin] || st[Module]) && neg[name][Enabled]:
			return nil, true
		case op == OpAnd && st[Enabled] && (st[Builtin] || st[Module]):
			drop[Var(name, Enabled).key] = true
		}
		if op != OpAnd {
			continue
		}
		// X=y excludes X=m and the other way round; X without one state is
		// the other.
		for _, pair := range [][2]State{{Builtin, Module}, {Module, Builtin}} {
			have, not := pair[0], pair[1]
			switch {
			case st[have] && neg[name][not]:
				drop[Not(Var(name, not)).key] = true
			case st[Enabled] && !st[Builtin] && !st[Module] && neg[name][not] && !neg[name][have]:
				drop[Var(name, Enabled).key], drop[Not(Var(name, not)).key] = true, true
				add = append(add, Var(name, have))
			}
		}
	}
	if op == OpAnd {
		for name, st := range neg {
			if st[Builtin] && st[Module] {
				drop[Not(Var(name, Builtin)).key], drop[Not(Var(name, Module)).key] = true, true
				add = append(add, Not(Var(name, Enabled)))
			}
		}
	}
	if len(drop) == 0 && len(add) == 0 {
		return args, false
	}
	for _, a := range args {
		if !drop[a.key] {
			out = append(out, a)
		}
	}
	return append(out, add...), false
}

// hoist factors the terms every argument of an Or shares:
// (A && T) || (B && T) becomes (A || B) && T. It returns nil when there
// is nothing to factor.
func hoist(args []*Expr) *Expr {
	if len(args) < 2 {
		return nil
	}
	terms := func(e *Expr) []*Expr {
		if e.Op == OpAnd {
			return e.Args
		}
		return []*Expr{e}
	}
	common := map[string]*Expr{}
	for _, t := range terms(args[0]) {
		common[t.key] = t
	}
	for _, a := range args[1:] {
		keep := map[string]*Expr{}
		for _, t := range terms(a) {
			if common[t.key] != nil {
				keep[t.key] = t
			}
		}
		common = keep
	}
	if len(common) == 0 {
		return nil
	}
	var rest, shared []*Expr
	for _, a := range args {
		var r []*Expr
		for _, t := range terms(a) {
			if common[t.key] == nil {
				r = append(r, t)
			}
		}
		rest = append(rest, And(r...))
	}
	for _, t := range common {
		shared = append(shared, t)
	}
	return And(append(shared, Or(rest...))...)
}

// absorb drops every argument whose dual-operator terms are a superset of
// another argument's: in an Or, A && B is dropped next to A.
func absorb(op Op, args []*Expr) []*Expr {
	dual := OpOr
	if op == OpOr {
		dual = OpAnd
	}
	terms := make([]map[string]bool, len(args))
	for i, a := range args {
		terms[i] = map[string]bool{}
		if a.Op == dual {
			for _, t := range a.Args {
				terms[i][t.key] = true
			}
		} else {
			terms[i][a.key] = true
		}
	}
	subset := func(a, b map[string]bool) bool {
		for k := range a {
			if !b[k] {
				return false
			}
		}
		return true
	}
	var out []*Expr
	for i, a := range args {
		absorbed := false
		for j := range args {
			if i != j && len(terms[j]) < len(terms[i]) && subset(terms[j], terms[i]) {
				absorbed = true
				break
			}
		}
		if !absorbed {
			out = append(out, a)
		}
	}
	return out
}

// String renders e in the syntax Parse accepts. Opaque atoms that are not
// identifiers, or are named true or false, are quoted.
func (e *Expr) String() string { return e.key }

func (e *Expr) render() string {
	switch e.Op {
	case OpConst:
		if e.Val {
			return "true"
		}
		return "false"
	case OpVar:
		switch e.State {
		case Builtin:
			return e.Name + "=y"
		case Module:
			return e.Name + "=m"
		}
		if !isIdent(e.Name) || e.Name == "true" || e.Name == "false" {
			return strconv.Quote(e.Name)
		}
		return e.Name
	case OpNot:
		return "!" + e.Args[0].operand()
	}
	sep := " && "
	if e.Op == OpOr {
		sep = " || "
	}
	parts := make([]string, len(e.Args))
	for i, a := range e.Args {
		parts[i] = a.operand()
	}
	return strings.Join(parts, sep)
}

func (e *Expr) operand() string {
	if e.Op == OpAnd || e.Op == OpOr {
		return "(" + e.key + ")"
	}
	return e.key
}

// Config maps the symbols set in a .config to 'y' or 'm'. Symbols that are
// not set are absent.
type Config map[string]byte

// ParseConfig reads a .config. Non-tristate symbols with a value count as
// 'y'.
func ParseConfig(r io.Reader) (Config, error) {
	cfg := Config{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		name, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || !IsSymbol(name) || val == "n" {
			continue
		}
		if val == "m" {
			cfg[name] = 'm'
		} else {
			cfg[name] = 'y'
		}
	}
	return cfg, sc.Err()
}

// Tri is a three-valued truth value.
type Tri uint8

const (
	False Tri = iota
	True
	Unknown
)

func (t Tri) String() string { return [...]string{"false", "true", "unknown"}[t] }

// Eval evaluates e against cfg in Kleene logic: opaque atoms are Unknown.
func (e *Expr) Eval(cfg Config) Tri {
	switch e.Op {
	case OpConst:
		if e.Val {
			return True
		}
		return False
	case OpVar:
		if !IsSymbol(e.Name) {
			return Unknown
		}
		return triOf(match(e.State, cfg[e.Name]))
	case OpNot:
		switch e.Args[0].Eval(cfg) {
		case True:
			return False
		case False:
			return True
		}
		return Unknown
	}
	short, res := False, True // And: False short-circuits
	if e.Op == OpOr {
		short, res = True, False
	}
	for _, a := range e.Args {
		switch a.Eval(cfg) {
		case short:
			return short
		case Unknown:
			res = Unknown
		}
	}
	return res
}

func triOf(b bool) Tri {
	if b {
		return True
	}
	return False
}

func match(st State, v byte) bool {
	switch st {
	case Builtin:
		return v == 'y'
	case Module:
		return v == 'm'
	}
	return v == 'y' || v == 'm'
}

// Symbols returns the sorted Kconfig symbols e refers to.
func (e *Expr) Symbols() []string { return e.vars(true) }

// Opaque returns the sorted opaque atoms e refers to.
func (e *Expr) Opaque() []string { return e.vars(false) }

func (e *Expr) vars(symbols bool) []string {
	set := map[string]bool{}
	e.walkVars(func(v *Expr) {
		if IsSymbol(v.Name) == symbols {
			set[v.Name] = true
		}
	})
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// maxRelaxAtoms bounds the atoms Relax enumerates.
const maxRelaxAtoms = 12

// Relax removes the opaque atoms of e by existential quantification: the
// result holds for a setting of the Kconfig symbols when e holds for some
// value of the atoms. Past maxRelaxAtoms atoms it gives up and returns true.
func Relax(e *Expr) *Expr {
	atoms := e.Opaque()
	if len(atoms) == 0 {
		return e
	}
	if len(atoms) > maxRelaxAtoms {
		return Const(true)
	}
	terms := make([]*Expr, 0, 1<<len(atoms))
	for mask := range 1 << len(atoms) {
		vals := map[string]bool{}
		for i, a := range atoms {
			vals[a] = mask&(1<<i) != 0
		}
		terms = append(terms, Subst(e, func(a string) *Expr {
			if v, ok := vals[a]; ok {
				return Const(v)
			}
			return nil
		}))
	}
	return Or(terms...)
}

// Subst replaces each opaque atom of e by f's expression for it; one f
// returns nil for stays.
func Subst(e *Expr, f func(atom string) *Expr) *Expr {
	return Rewrite(e, func(v *Expr) *Expr {
		if IsSymbol(v.Name) {
			return nil
		}
		return f(v.Name)
	})
}

// With returns e with atom replaced by the constant v.
func With(e *Expr, atom string, v bool) *Expr {
	return Subst(e, func(a string) *Expr {
		if a == atom {
			return Const(v)
		}
		return nil
	})
}

// Rewrite replaces each variable of e, a symbol test or an opaque atom, by
// f's expression for it; one f returns nil for stays.
func Rewrite(e *Expr, f func(v *Expr) *Expr) *Expr {
	switch e.Op {
	case OpConst:
		return e
	case OpVar:
		if r := f(e); r != nil {
			return r
		}
		return e
	case OpNot:
		return Not(Rewrite(e.Args[0], f))
	}
	args := make([]*Expr, len(e.Args))
	for i, a := range e.Args {
		args[i] = Rewrite(a, f)
	}
	if e.Op == OpAnd {
		return And(args...)
	}
	return Or(args...)
}

// maxTableSymbols bounds the truth tables Implies enumerates.
const maxTableSymbols = 20

// Implies reports whether a implies b for every assignment, treating opaque
// atoms as free booleans. ok is false when the variables are too many to
// enumerate.
func Implies(a, b *Expr) (implies, ok bool) {
	vars := slices.Compact(slices.Sorted(slices.Values(append(
		append(a.Symbols(), b.Symbols()...), append(a.Opaque(), b.Opaque()...)...))))
	if len(vars) > maxTableSymbols {
		return false, false
	}
	// Opaque atoms and symbols tested only as enabled range over two
	// values; symbols tested for =y or =m range over n, y and m.
	vals := make([][]byte, len(vars))
	tri := map[string]bool{}
	for _, e := range []*Expr{a, b} {
		e.walkVars(func(v *Expr) {
			if v.State != Enabled {
				tri[v.Name] = true
			}
		})
	}
	n := 1
	for i, v := range vars {
		vals[i] = []byte{0, 'y'}
		if tri[v] {
			vals[i] = []byte{0, 'y', 'm'}
		}
		n *= len(vals[i])
		if n > 1<<maxTableSymbols {
			return false, false
		}
	}
	cfg := Config{}
	for row := range n {
		for i, v := range vars {
			cfg[v] = vals[i][row%len(vals[i])]
			row /= len(vals[i])
		}
		if a.eval2(cfg) && !b.eval2(cfg) {
			return false, true
		}
	}
	return true, true
}

// Equal reports whether a and b are equivalent; see Implies.
func Equal(a, b *Expr) (equal, ok bool) {
	ab, ok := Implies(a, b)
	if !ok || !ab {
		return false, ok
	}
	return Implies(b, a)
}

func (e *Expr) walkVars(f func(*Expr)) {
	if e.Op == OpVar {
		f(e)
	}
	for _, a := range e.Args {
		a.walkVars(f)
	}
}

// eval2 evaluates e with opaque atoms assigned in cfg like symbols.
func (e *Expr) eval2(cfg Config) bool {
	switch e.Op {
	case OpConst:
		return e.Val
	case OpVar:
		return match(e.State, cfg[e.Name])
	case OpNot:
		return !e.Args[0].eval2(cfg)
	case OpAnd:
		for _, a := range e.Args {
			if !a.eval2(cfg) {
				return false
			}
		}
		return true
	}
	for _, a := range e.Args {
		if a.eval2(cfg) {
			return true
		}
	}
	return false
}

// Parse parses the syntax String renders: identifiers, optionally suffixed
// with =y or =m, quoted opaque atoms, !, &&, ||, parentheses, true and
// false.
func Parse(s string) (*Expr, error) {
	p := &parser{s: s}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.skipSpace(); p.pos < len(p.s) {
		return nil, p.errorf("unexpected %q", p.s[p.pos:])
	}
	return e, nil
}

type parser struct {
	s   string
	pos int
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("kcond: parse %q at %d: %s", p.s, p.pos, fmt.Sprintf(format, args...))
}

func (p *parser) skipSpace() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t') {
		p.pos++
	}
}

func (p *parser) accept(tok string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.s[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *parser) or() (*Expr, error) {
	return p.list(OpOr, "||", p.and)
}

func (p *parser) and() (*Expr, error) {
	return p.list(OpAnd, "&&", p.unary)
}

func (p *parser) list(op Op, tok string, next func() (*Expr, error)) (*Expr, error) {
	e, err := next()
	if err != nil {
		return nil, err
	}
	args := []*Expr{e}
	for p.accept(tok) {
		e, err := next()
		if err != nil {
			return nil, err
		}
		args = append(args, e)
	}
	return nary(op, args), nil
}

func (p *parser) unary() (*Expr, error) {
	switch {
	case p.accept("!"):
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		return Not(e), nil
	case p.accept("("):
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if !p.accept(")") {
			return nil, p.errorf("missing )")
		}
		return e, nil
	}
	if p.pos < len(p.s) && p.s[p.pos] == '"' {
		q, err := strconv.QuotedPrefix(p.s[p.pos:])
		if err != nil {
			return nil, p.errorf("bad quoted atom")
		}
		p.pos += len(q)
		name, _ := strconv.Unquote(q)
		return Var(name, Enabled), nil
	}
	start := p.pos
	for p.pos < len(p.s) && isIdentByte(p.s[p.pos]) {
		p.pos++
	}
	name := p.s[start:p.pos]
	switch name {
	case "":
		return nil, p.errorf("expected an operand")
	case "true", "false":
		return Const(name == "true"), nil
	}
	st := Enabled
	if strings.HasPrefix(p.s[p.pos:], "=") {
		switch {
		case strings.HasPrefix(p.s[p.pos:], "=y"):
			st = Builtin
		case strings.HasPrefix(p.s[p.pos:], "=m"):
			st = Module
		default:
			return nil, p.errorf("expected =y or =m")
		}
		p.pos += 2
	}
	return Var(name, st), nil
}

func isIdent(s string) bool {
	for i := range len(s) {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return s != ""
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
