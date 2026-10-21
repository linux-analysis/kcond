package kcond

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// knownMacros are macros whose definedness is fixed when compiling kernel
// code.
var knownMacros = map[string]bool{
	"__KERNEL__":   true,
	"__CHECKER__":  false,
	"__GENKSYMS__": false,
	"__cplusplus":  false,
}

// Code tells how a file is compiled, which decides some of the macros the
// compiler and Kbuild define in it.
type Code struct {
	// Kernel is set for a source compiled only into the kernel, at the
	// configuration's word size, so that arch macros follow Kconfig (see
	// ParseIf).
	Kernel bool
	// Assembly tells whether __ASSEMBLY__ is defined: True in assembly
	// files, which Kbuild compiles with -D__ASSEMBLY__, False in C, and
	// Unknown in headers, which both include.
	Assembly Tri
	// Rust is set for a Rust source, whose regions are its #[cfg]
	// attributes rather than preprocessor directives.
	Rust bool
}

// assemblyAtom stands for __ASSEMBLY__ where it is unknown.
const assemblyAtom = "defined(__ASSEMBLY__)"

// assemblyMacro reports whether name is defined exactly in assembly files:
// __ASSEMBLY__, or __ASSEMBLER__, which the compiler defines for them.
func assemblyMacro(name string) bool { return name == "__ASSEMBLY__" || name == "__ASSEMBLER__" }

// archMacros are macros the compiler or an arch Makefile defines exactly
// when a Kconfig symbol is set, in kernel code: __x86_64__ and __i386__
// come with the x86 word size, -D__arch_um__ with user-mode Linux.
var archMacros = map[string]string{"__x86_64__": "CONFIG_X86_64", "__i386__": "CONFIG_X86_32", "__arch_um__": "CONFIG_UML"}

// comparisons maps a C comparison operator to the results of cmp.Compare
// for which it holds.
var comparisons = map[string][]int{"==": {0}, "!=": {-1, 1}, "<": {-1}, "<=": {-1, 0}, ">": {1}, ">=": {0, 1}}

// maxDefinition bounds the text of a macro's tracked definition (see
// Lines). A file that redefines a macro under tests of itself, as arm's
// glue-proc.h does CPU_NAME once per CPU, would double it with each one.
const maxDefinition = 4096

// Lines returns the preprocessor condition of every line of a C source:
// lines[i] is the condition under which line i+1 is compiled. #if, #elif
// and #else lines get the condition of the branch they open, so that a
// change wrapping code in #ifdef CONFIG_X reads as one to CONFIG_X; #endif
// gets that of the branch it closes; other directives that of their region.
// Conditions that ParseIf rejects become opaque atoms; unbalanced directives are an
// error. An include guard (a leading #ifndef X followed by #define X) adds no
// condition. A macro the file itself #defines or #undefs is known after
// that, where it would otherwise be an opaque atom: defined there, or not,
// under the condition of the directive's line, and in a C or assembly
// source undefined before the first #include, as nothing else could have
// defined it (qspinlock.c tests the macro it defines before including
// itself again), though in a header the file including it may have (as
// for CREATE_TRACE_POINTS in define_trace.h), unless the header defines
// it right after testing it, as a guard; an #include may define it
// again; a definition past maxDefinition reads as the atom again. code
// tells how the file is compiled; a header is compiled into C and assembly
// units alike, so its lines count where either compiles them.
func Lines(src []byte, code Code) ([]*Expr, error) {
	lines := strings.Split(string(src), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	defs := map[string]*Expr{} // the macros the file defines or undefines
	test := ""                 // the #if or #ifndef directive just before
	for inComment, i := false, 0; i < len(lines); i++ {
		var text string
		text, inComment = StripComments(lines[i], inComment)
		if d, ok := strings.CutPrefix(strings.TrimSpace(text), "#"); ok {
			name, rest := splitDirective(d)
			if id := firstIdent(rest); name == "define" && fileMacro(id) {
				// In a header only a guard: defined right after its test.
				if toks, _ := lexIf(test); code.Assembly != Unknown || slices.Contains(toks, id) {
					defs[id] = Const(false)
				}
			}
			test = ""
			if name == "if" || name == "ifndef" {
				test = rest
			}
		}
	}
	type frame struct {
		prev    []*Expr // conditions of the earlier branches
		cur     *Expr
		sawElse bool
	}
	conds := make([]*Expr, len(lines))
	var stack []frame
	// levels[k] is the condition inside the outermost k frames.
	levels := []*Expr{Const(true)}
	update := func() {
		levels = levels[:1]
		for _, f := range stack {
			terms := []*Expr{levels[len(levels)-1], f.cur}
			for _, p := range f.prev {
				terms = append(terms, Not(p))
			}
			levels = append(levels, And(terms...))
		}
	}
	setConds := func(from, to int, c *Expr) {
		for j := from; j <= to; j++ {
			conds[j] = c
		}
	}
	inComment, first, guard, guardLine := false, true, "", 0
	track := func(id string, e *Expr) {
		if len(e.String()) > maxDefinition {
			delete(defs, id)
		} else {
			defs[id] = e
		}
	}
	for i := 0; i < len(lines); i++ {
		conds[i] = levels[len(stack)]
		var text string
		text, inComment = StripComments(lines[i], inComment)
		d, ok := strings.CutPrefix(strings.TrimSpace(text), "#")
		if !ok {
			continue
		}
		start := i
		for strings.HasSuffix(strings.TrimRight(lines[i], " \t\r"), "\\") && i+1 < len(lines) {
			d = strings.TrimSuffix(strings.TrimRight(d, " \t\r"), "\\")
			i++
			conds[i] = levels[len(stack)]
			text, inComment = StripComments(lines[i], inComment)
			d += " " + text
		}
		name, rest := splitDirective(d)
		errorf := func(format string, args ...any) error {
			return fmt.Errorf("kcond: line %d: #%s: %s", i+1, name, fmt.Sprintf(format, args...))
		}
		if name != "define" {
			guard = ""
		}
		opens := false // the line opens a branch and gets its condition
		switch name {
		case "if", "ifdef", "ifndef":
			c, err := directiveCond(name, rest, code, defs)
			if err != nil {
				return nil, errorf("%v", err)
			}
			if name == "ifndef" && first {
				guard, guardLine = firstIdent(rest), start
			}
			stack = append(stack, frame{cur: c})
			opens = true
		case "elif", "elifdef", "elifndef", "else":
			if len(stack) == 0 {
				return nil, errorf("no matching #if")
			}
			top := &stack[len(stack)-1]
			if top.sawElse {
				return nil, errorf("after #else")
			}
			c := Const(true)
			if name == "else" {
				top.sawElse = true
			} else {
				var err error
				if c, err = directiveCond(strings.TrimPrefix(name, "el"), rest, code, defs); err != nil {
					return nil, errorf("%v", err)
				}
			}
			top.prev = append(top.prev, top.cur)
			top.cur = c
			opens = true
		case "endif":
			if len(stack) == 0 {
				return nil, errorf("no matching #if")
			}
			stack = stack[:len(stack)-1]
		case "define", "undef":
			id := firstIdent(rest)
			if name == "define" && guard != "" && id == guard {
				stack[len(stack)-1].cur = Const(true)
				update()
				setConds(guardLine, i, levels[len(stack)])
			} else if fileMacro(id) {
				level, prev := levels[len(stack)], macroDefs(id, code, defs)
				if name == "define" {
					track(id, Or(level, prev))
				} else {
					track(id, And(Not(level), prev))
				}
			}
			guard = ""
		case "include":
			for id, e := range defs {
				track(id, Or(e, And(levels[len(stack)], macro(id, code))))
			}
		}
		first = false
		update()
		if opens {
			setConds(start, i, levels[len(stack)])
		}
	}
	if len(stack) > 0 {
		return nil, fmt.Errorf("kcond: %d unterminated #if", len(stack))
	}
	if code.Assembly == Unknown {
		either := map[*Expr]*Expr{}
		for i, c := range conds {
			e, ok := either[c]
			if !ok {
				e = c
				if slices.Contains(c.Opaque(), assemblyAtom) {
					e = Or(With(c, assemblyAtom, true), With(c, assemblyAtom, false))
				}
				either[c] = e
			}
			conds[i] = e
		}
	}
	return conds, nil
}

// splitDirective splits the text after # into the directive name and the
// rest.
func splitDirective(d string) (name, rest string) {
	d = strings.TrimSpace(d)
	name = firstIdent(d)
	return name, strings.TrimSpace(d[len(name):])
}

func firstIdent(s string) string {
	n := 0
	for n < len(s) && isIdentByte(s[n]) {
		n++
	}
	return s[:n]
}

// directiveCond returns the condition of an #if, #ifdef or #ifndef line;
// defs are the macros the file defined or undefined before it.
func directiveCond(name, rest string, code Code, defs map[string]*Expr) (*Expr, error) {
	if name == "if" {
		if rest == "" {
			return nil, fmt.Errorf("missing condition")
		}
		e, err := parseIf(rest, code, defs)
		if err != nil {
			return Var(strings.Join(strings.Fields(rest), " "), Enabled), nil
		}
		return e, nil
	}
	id := firstIdent(rest)
	if id == "" || id[0] >= '0' && id[0] <= '9' {
		return nil, fmt.Errorf("missing macro name")
	}
	if name == "ifndef" {
		return Not(macroDefs(id, code, defs)), nil
	}
	return macroDefs(id, code, defs), nil
}

// fileMacro reports whether the definition of the macro name, done or
// undone by the file itself, is worth tracking: one that is neither a
// Kconfig symbol nor a macro with a fixed or Kconfig meaning, which would
// otherwise be an opaque atom.
func fileMacro(name string) bool {
	_, known := knownMacros[name]
	_, arch := archMacros[name]
	return name != "" && !known && !arch && !assemblyMacro(name) && !IsSymbol(name)
}

// macroDefs is macro, with the definitions of the file's own macros.
func macroDefs(name string, code Code, defs map[string]*Expr) *Expr {
	if e, ok := defs[name]; ok {
		return e
	}
	return macro(name, code)
}

// StripComments replaces the comments in line with spaces. inComment tells
// whether line starts inside a block comment; the result tells whether the
// next line does. String and character literals end at the end of the
// line.
func StripComments(line string, inComment bool) (string, bool) {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inComment:
			if c == '*' && i+1 < len(line) && line[i+1] == '/' {
				inComment = false
				i++
				b.WriteByte(' ')
			}
		case quote != 0:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(line) {
				i++
				b.WriteByte(line[i])
			} else if c == quote {
				quote = 0
			}
		case c == '/' && i+1 < len(line) && line[i+1] == '*':
			inComment = true
			i++
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return b.String(), false
		default:
			if c == '"' || c == '\'' {
				quote = c
			}
			b.WriteByte(c)
		}
	}
	return b.String(), inComment
}

// ParseIf converts a C preprocessor #if expression. defined(), IS_ENABLED(),
// IS_BUILTIN(), IS_MODULE(), IS_REACHABLE() (approximated as IS_ENABLED),
// config_enabled() (IS_BUILTIN before v4.9) and bare CONFIG_ symbols map to
// tristate variables; a CONFIG_X_MODULE macro means X=m. Integer literals are constants. Arch macros such as
// __i386__ and comparisons of BITS_PER_LONG with an integer map to the Kconfig
// symbols they follow, and __ASSEMBLY__ to what code tells of the file (see
// Code and macro). Any other operand, such as a comparison or arithmetic,
// becomes an opaque atom named by its space-separated tokens.
func ParseIf(s string, code Code) (*Expr, error) { return parseIf(s, code, nil) }

// parseIf is ParseIf with defs, the definitions of the macros the file
// defines or undefines before the line.
func parseIf(s string, code Code, defs map[string]*Expr) (*Expr, error) {
	toks, err := lexIf(s)
	if err != nil {
		return nil, err
	}
	p := &cparser{toks: toks, code: code, defs: defs}
	n, err := p.ternary()
	if err != nil {
		return nil, err
	}
	if p.pos < len(toks) {
		return nil, fmt.Errorf("kcond: parse %q: unexpected %q", s, toks[p.pos])
	}
	return p.conv(n), nil
}

var cOps = []string{"||", "&&", "==", "!=", "<=", ">=", "<<", ">>",
	"!", "(", ")", ",", "<", ">", "+", "-", "*", "/", "%", "&", "|", "^", "~", "?", ":"}

func lexIf(s string) ([]string, error) {
	var toks []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case isIdentByte(c):
			j := i
			for j < len(s) && isIdentByte(s[j]) {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		case c == '\'':
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("kcond: parse %q: unterminated character literal", s)
			}
			toks = append(toks, s[i:j+1])
			i = j + 1
		default:
			op := ""
			for _, o := range cOps {
				if strings.HasPrefix(s[i:], o) {
					op = o
					break
				}
			}
			if op == "" {
				return nil, fmt.Errorf("kcond: parse %q: unexpected %q", s, c)
			}
			toks = append(toks, op)
			i += len(op)
		}
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("kcond: parse %q: empty expression", s)
	}
	return toks, nil
}

// cnode is a parsed #if operand spanning toks[lo:hi].
type cnode struct {
	kind   string // "||", "&&", "!", "()", "defined", "call", "id", "num", "other"
	name   string
	kids   []*cnode
	lo, hi int
}

type cparser struct {
	toks []string
	pos  int
	code Code
	defs map[string]*Expr
}

func (p *cparser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *cparser) expect(tok string) error {
	if p.peek() != tok {
		return fmt.Errorf("kcond: parse %q: expected %q", strings.Join(p.toks, " "), tok)
	}
	p.pos++
	return nil
}

func (p *cparser) ternary() (*cnode, error) {
	lo := p.pos
	c, err := p.binary(0)
	if err != nil || p.peek() != "?" {
		return c, err
	}
	p.pos++
	if _, err := p.ternary(); err != nil {
		return nil, err
	}
	if err := p.expect(":"); err != nil {
		return nil, err
	}
	if _, err := p.ternary(); err != nil {
		return nil, err
	}
	return &cnode{kind: "other", lo: lo, hi: p.pos}, nil
}

// cLevels lists the binary operators by increasing precedence.
var cLevels = [][]string{{"||"}, {"&&"}, {"|"}, {"^"}, {"&"}, {"==", "!="},
	{"<", ">", "<=", ">="}, {"<<", ">>"}, {"+", "-"}, {"*", "/", "%"}}

func (p *cparser) binary(level int) (*cnode, error) {
	if level == len(cLevels) {
		return p.unary()
	}
	lo := p.pos
	n, err := p.binary(level + 1)
	if err != nil {
		return nil, err
	}
	for slices.Contains(cLevels[level], p.peek()) {
		op := p.peek()
		p.pos++
		r, err := p.binary(level + 1)
		if err != nil {
			return nil, err
		}
		kind := "other"
		if op == "||" || op == "&&" {
			kind = op
		}
		n = &cnode{kind: kind, kids: []*cnode{n, r}, lo: lo, hi: p.pos}
	}
	return n, nil
}

func (p *cparser) unary() (*cnode, error) {
	lo := p.pos
	switch tok := p.peek(); tok {
	case "!", "~", "-", "+":
		p.pos++
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		kind := "other"
		if tok == "!" {
			kind = "!"
		}
		return &cnode{kind: kind, kids: []*cnode{n}, lo: lo, hi: p.pos}, nil
	case "(":
		p.pos++
		n, err := p.ternary()
		if err != nil {
			return nil, err
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		return &cnode{kind: "()", kids: []*cnode{n}, lo: lo, hi: p.pos}, nil
	case "":
		return nil, fmt.Errorf("kcond: parse %q: expected an operand", strings.Join(p.toks, " "))
	}
	tok := p.peek()
	p.pos++
	switch {
	case tok == "defined":
		paren := p.peek() == "("
		if paren {
			p.pos++
		}
		id := p.peek()
		if id == "" || !isIdentByte(id[0]) {
			return nil, fmt.Errorf("kcond: parse %q: defined needs a macro name", strings.Join(p.toks, " "))
		}
		p.pos++
		if paren {
			if err := p.expect(")"); err != nil {
				return nil, err
			}
		}
		return &cnode{kind: "defined", name: id, lo: lo, hi: p.pos}, nil
	case tok[0] >= '0' && tok[0] <= '9':
		return &cnode{kind: "num", name: tok, lo: lo, hi: p.pos}, nil
	case tok[0] == '\'':
		return &cnode{kind: "other", lo: lo, hi: p.pos}, nil
	case !isIdentByte(tok[0]):
		return nil, fmt.Errorf("kcond: parse %q: unexpected %q", strings.Join(p.toks, " "), tok)
	}
	if p.peek() != "(" {
		return &cnode{kind: "id", name: tok, lo: lo, hi: p.pos}, nil
	}
	p.pos++
	var args []*cnode
	for p.peek() != ")" {
		a, err := p.ternary()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		if p.peek() != "," {
			break
		}
		p.pos++
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return &cnode{kind: "call", name: tok, kids: args, lo: lo, hi: p.pos}, nil
}

func (p *cparser) conv(n *cnode) *Expr {
	switch n.kind {
	case "||":
		return Or(p.conv(n.kids[0]), p.conv(n.kids[1]))
	case "&&":
		return And(p.conv(n.kids[0]), p.conv(n.kids[1]))
	case "!":
		return Not(p.conv(n.kids[0]))
	case "()":
		return p.conv(n.kids[0])
	case "defined":
		return macroDefs(n.name, p.code, p.defs)
	case "id":
		if IsSymbol(n.name) {
			return defined(n.name)
		}
	case "num":
		v, err := cInt(n.name)
		if err == nil {
			return Const(v != 0)
		}
	case "call":
		if len(n.kids) == 1 && n.kids[0].kind == "id" && IsSymbol(n.kids[0].name) {
			if e, ok := macroCall(n.name, n.kids[0].name); ok {
				return e
			}
		}
	case "other":
		if e, ok := p.bitsPerLong(n); ok {
			return e
		}
		if e, ok := p.pageSize(n); ok {
			return e
		}
	}
	return Var(strings.Join(p.toks[n.lo:n.hi], " "), Enabled)
}

// cInt parses a C integer literal such as 0x10 or 1UL.
func cInt(tok string) (uint64, error) {
	return strconv.ParseUint(strings.TrimRight(tok, "uUlL"), 0, 64)
}

// bitsPerLong converts a comparison of BITS_PER_LONG with an integer:
// asm-generic/bitsperlong.h makes it 64 with CONFIG_64BIT, 32 without.
// Outside kernel code the atom BITS_PER_LONG == 32 stands for a 64-bit
// configuration compiling the line with a 32-bit long, as its 32-bit vDSO
// does.
func (p *cparser) bitsPerLong(n *cnode) (*Expr, bool) {
	if len(n.kids) != 2 {
		return nil, false
	}
	a, b := n.kids[0], n.kids[1]
	results, ok := comparisons[p.toks[a.hi]]
	sign := 1
	if a.kind == "num" {
		a, b, sign = b, a, -1
	}
	if !ok || a.kind != "id" || a.name != "BITS_PER_LONG" || b.kind != "num" {
		return nil, false
	}
	v, err := cInt(b.name)
	if err != nil {
		return nil, false
	}
	holds := func(bits uint64) bool { return slices.Contains(results, sign*cmp.Compare(bits, v)) }
	long64, long32 := Var("CONFIG_64BIT", Enabled), Not(Var("CONFIG_64BIT", Enabled))
	if !p.code.Kernel {
		atom := Var("BITS_PER_LONG == 32", Enabled)
		long64, long32 = And(long64, Not(atom)), Or(long32, atom)
	}
	switch at64, at32 := holds(64), holds(32); {
	case at64 == at32:
		return Const(at64), true
	case at64:
		return long64, true
	}
	return long32, true
}

// flipped maps a comparison operator to the one that holds with its
// operands swapped.
var flipped = map[string]string{"==": "==", "!=": "!=", "<": ">", "<=": ">=", ">": "<", ">=": "<="}

// sizeMacro matches the size constants of linux/sizes.h, such as SZ_64K.
var sizeMacro = regexp.MustCompile(`^SZ_([0-9]+)([KMG])$`)

// pageSize converts a comparison of PAGE_SIZE or PAGE_SHIFT with a size
// into the atom PAGE_SIZE <op> <bytes>, which a build decides from its
// architecture's page sizes (see PageSizeHolds). A size is an integer, an
// SZ_ constant or XEN_PAGE_SIZE, which is 4 KiB.
func (p *cparser) pageSize(n *cnode) (*Expr, bool) {
	if len(n.kids) != 2 {
		return nil, false
	}
	a, b := n.kids[0], n.kids[1]
	op := p.toks[a.hi]
	if _, ok := comparisons[op]; !ok {
		return nil, false
	}
	page := func(m *cnode) bool { return m.kind == "id" && (m.name == "PAGE_SIZE" || m.name == "PAGE_SHIFT") }
	if !page(a) {
		a, b, op = b, a, flipped[op]
	}
	if !page(a) {
		return nil, false
	}
	var v uint64
	switch {
	case b.kind == "num":
		var err error
		if v, err = cInt(b.name); err != nil {
			return nil, false
		}
	case a.name == "PAGE_SHIFT":
		return nil, false
	case b.kind == "id" && b.name == "XEN_PAGE_SIZE":
		v = 4096
	case b.kind == "id" && sizeMacro.MatchString(b.name):
		m := sizeMacro.FindStringSubmatch(b.name)
		v, _ = strconv.ParseUint(m[1], 10, 64)
		v <<= map[string]uint{"K": 10, "M": 20, "G": 30}[m[2]]
	default:
		return nil, false
	}
	if a.name == "PAGE_SHIFT" {
		if v >= 64 {
			return nil, false
		}
		v = 1 << v
	}
	return Var(fmt.Sprintf("PAGE_SIZE %s %d", op, v), Enabled), true
}

// PageSizeHolds tells whether a page size atom, as ParseIf makes them,
// holds with pages of size bytes; ok is false for any other atom.
func PageSizeHolds(atom string, size uint64) (holds, ok bool) {
	f := strings.Fields(atom)
	if len(f) != 3 || f[0] != "PAGE_SIZE" {
		return false, false
	}
	results, ok := comparisons[f[1]]
	v, err := strconv.ParseUint(f[2], 10, 64)
	if !ok || err != nil {
		return false, false
	}
	return slices.Contains(results, cmp.Compare(size, v)), true
}

// macro returns the condition under which macro name is defined, with
// the arch macros mapped to their Kconfig symbols. Outside kernel code an
// x86-64 configuration may also compile the line as 16- or 32-bit code,
// where __i386__ is defined instead of __x86_64__: the atom
// defined(__i386__) stands for that. __ASSEMBLY__ follows code.Assembly.
func macro(name string, code Code) *Expr {
	if assemblyMacro(name) {
		if code.Assembly == Unknown {
			return Var(assemblyAtom, Enabled)
		}
		return Const(code.Assembly == True)
	}
	s, ok := archMacros[name]
	if !ok {
		return defined(name)
	}
	e := Var(s, Enabled)
	if code.Kernel || name == "__arch_um__" {
		return e
	}
	i386 := Var("defined(__i386__)", Enabled)
	if name == "__i386__" {
		return Or(e, And(Var("CONFIG_X86_64", Enabled), i386))
	}
	return And(e, Not(i386))
}

// defined returns the condition under which macro name is defined.
func defined(name string) *Expr {
	if v, ok := knownMacros[name]; ok {
		return Const(v)
	}
	if !IsSymbol(name) {
		return Var("defined("+name+")", Enabled)
	}
	if base, ok := strings.CutSuffix(name, "_MODULE"); ok && IsSymbol(base) {
		return Var(base, Module)
	}
	return Var(name, Builtin)
}
