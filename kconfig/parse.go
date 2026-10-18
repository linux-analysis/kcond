// Package kconfig reads Kconfig trees and computes which values each symbol
// can take on an architecture. It follows the language described in
// Documentation/kbuild/kconfig-language.rst; macro calls the kconfig tool
// would run through a shell or the compiler stay unevaluated.
package kconfig

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Env holds the make variables Kconfig files refer to, such as ARCH and
// SRCARCH. Old trees read them through "option env" symbols.
type Env map[string]string

// Type is the type of a symbol.
type Type uint8

const (
	Unknown Type = iota // referenced but never defined
	Bool
	Tristate
	String
	Int
	Hex
)

var typeNames = map[string]Type{"bool": Bool, "boolean": Bool, "tristate": Tristate, "string": String, "int": Int, "hex": Hex,
	"def_bool": Bool, "def_boolean": Bool, "def_tristate": Tristate}

// Symbol is a Kconfig symbol with the properties of all its definitions.
// Every condition already includes the dependencies of the definition it
// comes from, with nil standing for y.
type Symbol struct {
	Name  string
	Type  Type
	Files []string // where it is defined
	// Prompts are the visibility conditions of the prompts.
	Prompts []*Expr
	// Defaults are in parse order; the first whose condition holds wins.
	Defaults []Default
	// RevDep are the select conditions, each including the selecting
	// symbol; Implied are those of imply.
	RevDep, Implied []*Expr
	// DirDep are the dependencies of the definitions outside a choice.
	DirDep       []*Expr
	Choice       *Choice
	Env          string // the variable an "option env" symbol reads
	Transitional bool
}

// Default is a default value and the condition under which it applies.
type Default struct {
	Value, Cond *Expr
}

// Choice is a choice block.
type Choice struct {
	Members  []*Symbol
	Optional bool // all members may be n (before v6.11)
	Tristate bool // members may be m together (before v6.11)
	typed    bool // the choice declares its type
}

// Tree is the Kconfig of one architecture.
type Tree struct {
	Syms map[string]*Symbol
	// Modules names the symbol that enables modules.
	Modules  string
	Env      Env
	Files    []string // in the order read
	Warnings []string
}

// Parse reads the Kconfig tree for the architecture env["SRCARCH"]
// starting at the top-level Kconfig, or at arch/SRCARCH/Kconfig in trees
// that have none. Problems in the files are recorded as warnings; only a
// missing root is an error.
func Parse(fsys fs.FS, env Env) (*Tree, error) {
	root := "Kconfig"
	if _, err := fs.Stat(fsys, root); err != nil {
		root = path.Join("arch", env["SRCARCH"], "Kconfig")
		if _, err := fs.Stat(fsys, root); err != nil {
			return nil, fmt.Errorf("kconfig: no Kconfig for %q", env["SRCARCH"])
		}
	}
	p := &parser{fsys: fsys, t: &Tree{Syms: map[string]*Symbol{}, Env: env}, reading: map[string]bool{}}
	p.file(root)
	p.end()
	if n := len(p.blocks); n > 0 {
		p.warnf(root, 0, "%d unterminated blocks", n)
	}
	if p.t.Modules == "" && p.t.Syms["MODULES"] != nil {
		p.t.Modules = "MODULES"
	}
	return p.t, nil
}

// block is an if, menu or choice block whose conditions apply to the
// entries inside.
type block struct {
	kind   string // "if", "menu" or "choice"
	dep    *Expr
	vis    *Expr // menu: visible if
	choice *Choice
}

type nameCond struct {
	name string
	cond *Expr
}

// entry is the config, menu, choice or comment entry being read.
type entry struct {
	kind       string
	sym        *Symbol
	blk        *block // menu and choice
	dep        *Expr
	typ        Type
	prompt     bool
	promptCond *Expr
	defaults   []Default
	selects    []nameCond
	implies    []nameCond
}

type parser struct {
	fsys    fs.FS
	t       *Tree
	blocks  []*block
	cur     *entry
	reading map[string]bool
}

func (p *parser) warnf(file string, line int, format string, args ...any) {
	p.t.Warnings = append(p.t.Warnings, fmt.Sprintf("%s:%d: ", file, line)+fmt.Sprintf(format, args...))
}

func (p *parser) sym(name string) *Symbol {
	s := p.t.Syms[name]
	if s == nil {
		s = &Symbol{Name: name}
		p.t.Syms[name] = s
	}
	return s
}

type line struct {
	no   int
	text string
}

// logicalLines splits a file into lines, joining backslash continuations.
func logicalLines(data []byte) []line {
	var out []line
	raw := strings.Split(string(data), "\n")
	for i := 0; i < len(raw); i++ {
		l := line{no: i + 1, text: raw[i]}
		for strings.HasSuffix(strings.TrimRight(l.text, " \t\r"), "\\") && i+1 < len(raw) {
			t := strings.TrimRight(l.text, " \t\r")
			i++
			l.text = t[:len(t)-1] + " " + raw[i]
		}
		out = append(out, l)
	}
	return out
}

// stripComment drops a # comment outside quotes.
func stripComment(s string) string {
	var q byte
	for i := 0; i < len(s); i++ {
		if k := macroEnd(s, i); k > 0 {
			i = k - 1
			continue
		}
		switch c := s[i]; {
		case q != 0:
			if c == '\\' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#':
			return s[:i]
		}
	}
	return s
}

// indentation returns the column of the first non-blank byte, with tabs
// at multiples of 8.
func indentation(s string) int {
	col := 0
	for _, c := range []byte(s) {
		switch c {
		case ' ':
			col++
		case '\t':
			col = col&^7 + 8
		default:
			return col
		}
	}
	return col
}

// skipHelp returns the index of the last line of the help text that
// starts after lines[i].
func skipHelp(lines []line, i int) int {
	indent := -1
	last := i
	for j := i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j].text) == "" {
			continue
		}
		ind := indentation(lines[j].text)
		if indent < 0 {
			if ind == 0 {
				return last
			}
			indent = ind
		} else if ind < indent {
			return last
		}
		last = j
	}
	return last
}

var assignRe = regexp.MustCompile(`^[A-Za-z0-9_-]+\s*(:=|\+=|=)`)

// expand substitutes the known $(VAR) references.
func (p *parser) expand(s string) string {
	if !strings.Contains(s, "$(") {
		return s
	}
	for k, v := range p.t.Env {
		s = strings.ReplaceAll(s, "$("+k+")", v)
	}
	return s
}

// helpRe matches ---help--- and the "--- help ---" some old trees use.
var helpRe = regexp.MustCompile(`^---\s*help\s*---$`)

var oldVarRe = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

func (p *parser) file(name string) {
	if p.reading[name] {
		p.warnf(name, 0, "recursive source")
		return
	}
	data, err := fs.ReadFile(p.fsys, name)
	if err != nil {
		p.warnf(name, 0, "%v", err)
		return
	}
	p.reading[name] = true
	defer delete(p.reading, name)
	p.t.Files = append(p.t.Files, name)
	lines := logicalLines(data)
	for i := 0; i < len(lines); i++ {
		text := strings.TrimSpace(p.expand(stripComment(lines[i].text)))
		if text == "" {
			continue
		}
		kw, rest := text, ""
		if j := strings.IndexAny(text, " \t"); j >= 0 {
			kw, rest = text[:j], strings.TrimSpace(text[j:])
		}
		if kw == "help" || helpRe.MatchString(text) {
			i = skipHelp(lines, i)
			continue
		}
		p.statement(name, lines[i].no, kw, rest, text)
	}
}

func (p *parser) statement(file string, no int, kw, rest, text string) {
	expr := func(s string, cond bool) *Expr {
		e, err := parseExpr(s)
		if err != nil {
			p.warnf(file, no, "%v", err)
			return &Expr{Op: OpOpaque, Name: "$(?)"}
		}
		if cond {
			e = rewriteM(e)
		}
		return e
	}
	// valueIf reads "<expr> [if <expr>]".
	valueIf := func(s string) (*Expr, *Expr) {
		v, c := splitIf(s)
		var cond *Expr
		if c != "" {
			cond = expr(c, true)
		}
		return expr(v, false), cond
	}
	switch kw {
	case "config", "menuconfig":
		p.end()
		name := strings.Fields(rest)
		if len(name) == 0 {
			p.warnf(file, no, "config without a name")
			return
		}
		s := p.sym(name[0])
		s.Files = append(s.Files, file)
		p.cur = &entry{kind: "config", sym: s}
		return
	case "choice":
		p.end()
		b := &block{kind: "choice", choice: &Choice{}}
		p.blocks = append(p.blocks, b)
		p.cur = &entry{kind: "choice", blk: b}
		return
	case "menu":
		p.end()
		b := &block{kind: "menu"}
		p.blocks = append(p.blocks, b)
		p.cur = &entry{kind: "menu", blk: b}
		return
	case "if":
		p.end()
		p.blocks = append(p.blocks, &block{kind: "if", dep: expr(rest, true)})
		return
	case "endchoice", "endmenu", "endif":
		p.end()
		p.pop(file, no, kw[3:])
		return
	case "comment":
		p.end()
		p.cur = &entry{kind: "comment"}
		return
	case "mainmenu":
		p.end()
		return
	case "source", "rsource", "osource", "orsource":
		p.end()
		p.source(file, no, kw, rest)
		return
	}

	if strings.HasPrefix(text, "$(") {
		return // a macro call run for its side effects, such as $(error-if,...)
	}
	e := p.cur
	if e == nil {
		if assignRe.MatchString(text) {
			return // a macro variable
		}
		p.warnf(file, no, "unexpected %q", kw)
		return
	}
	switch kw {
	case "bool", "boolean", "tristate", "string", "int", "hex":
		e.typ = typeNames[kw]
		if rest != "" {
			p.prompt(file, no, e, rest, expr)
		}
	case "def_bool", "def_boolean", "def_tristate":
		e.typ = typeNames[kw]
		v, c := valueIf(rest)
		e.defaults = append(e.defaults, Default{v, c})
	case "prompt":
		p.prompt(file, no, e, rest, expr)
	case "default":
		v, c := valueIf(rest)
		e.defaults = append(e.defaults, Default{v, c})
	case "depends", "requires":
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "on "))
		d, c := splitIf(rest)
		dep := expr(d, true)
		if c != "" {
			dep = or(dep, &Expr{Op: OpNo, L: expr(c, true)})
		}
		e.dep = and(e.dep, dep)
	case "select", "enable", "imply":
		n, c := splitIf(rest)
		nc := nameCond{name: strings.TrimSpace(n)}
		if c != "" {
			nc.cond = expr(c, true)
		}
		if kw == "imply" {
			e.implies = append(e.implies, nc)
		} else {
			e.selects = append(e.selects, nc)
		}
	case "visible":
		if e.blk != nil && e.blk.kind == "menu" {
			e.blk.vis = and(e.blk.vis, expr(strings.TrimSpace(strings.TrimPrefix(rest, "if")), true))
		}
	case "option":
		switch {
		case strings.HasPrefix(rest, "env="):
			if e.sym != nil {
				_, v, _ := quoted(rest, len("env="))
				e.sym.Env = v
			}
		case rest == "modules":
			if e.sym != nil {
				p.t.Modules = e.sym.Name
			}
		}
	case "modules":
		if e.sym != nil {
			p.t.Modules = e.sym.Name
		}
	case "optional":
		if e.blk != nil && e.blk.choice != nil {
			e.blk.choice.Optional = true
		}
	case "transitional":
		if e.sym != nil {
			e.sym.Transitional = true
		}
	case "range":
	default:
		p.warnf(file, no, "unexpected %q", kw)
	}
}

// prompt reads `"text" [if <expr>]`.
func (p *parser) prompt(file string, no int, e *entry, s string, expr func(string, bool) *Expr) {
	if s == "" || (s[0] != '"' && s[0] != '\'') {
		p.warnf(file, no, "bad prompt %q", s)
		return
	}
	j, _, err := quoted(s, 0)
	if err != nil {
		p.warnf(file, no, "%v", err)
		return
	}
	e.prompt = true
	if c := strings.TrimSpace(s[j:]); c != "" {
		e.promptCond = expr(strings.TrimSpace(strings.TrimPrefix(c, "if")), true)
	}
}

// splitIf splits "<expr> if <cond>" at the first top-level "if".
func splitIf(s string) (string, string) {
	depth := 0
	var q byte
	for i := 0; i < len(s); i++ {
		if k := macroEnd(s, i); k > 0 {
			i = k - 1
			continue
		}
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && c == 'i' && strings.HasPrefix(s[i:], "if") && (i == 0 || !isWordByte(s[i-1])) &&
			(i+2 == len(s) || !isWordByte(s[i+2])):
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:])
		}
	}
	return strings.TrimSpace(s), ""
}

func (p *parser) pop(file string, no int, kind string) {
	for i := len(p.blocks) - 1; i >= 0; i-- {
		if p.blocks[i].kind == kind {
			if i != len(p.blocks)-1 {
				p.warnf(file, no, "end%s closes %d inner blocks", kind, len(p.blocks)-1-i)
			}
			p.blocks = p.blocks[:i]
			return
		}
	}
	p.warnf(file, no, "end%s without %s", kind, kind)
}

func (p *parser) source(file string, no int, kw, rest string) {
	name := rest
	if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
		if _, q, err := quoted(rest, 0); err == nil {
			name = q
		}
	}
	name = oldVarRe.ReplaceAllStringFunc(name, func(m string) string {
		if v, ok := p.t.Env[m[1:]]; ok {
			return v
		}
		return m
	})
	if strings.HasPrefix(kw, "r") || kw == "orsource" {
		name = path.Join(path.Dir(file), name)
	}
	optional := strings.HasPrefix(kw, "o")
	if strings.ContainsAny(name, "*?[") {
		matches, _ := fs.Glob(p.fsys, name)
		slices.Sort(matches)
		if len(matches) == 0 && !optional {
			p.warnf(file, no, "no files match %q", name)
		}
		for _, m := range matches {
			p.file(m)
		}
		return
	}
	if _, err := fs.Stat(p.fsys, name); err != nil {
		if !optional {
			p.warnf(file, no, "missing %q", name)
		}
		return
	}
	p.file(name)
}

// end applies the entry being read to its symbol.
func (p *parser) end() {
	e := p.cur
	p.cur = nil
	if e == nil {
		return
	}
	switch e.kind {
	case "menu":
		e.blk.dep = e.dep
		return
	case "choice":
		e.blk.dep = e.dep
		e.blk.choice.Tristate = e.typ == Tristate
		e.blk.choice.typed = e.typ != Unknown
		return
	case "comment":
		return
	}
	var base, vis *Expr
	for _, b := range p.blocks {
		base = and(base, b.dep)
		vis = and(vis, b.vis)
	}
	base = and(base, e.dep)
	s := e.sym
	if s.Type == Unknown {
		s.Type = e.typ
	}
	if e.prompt {
		s.Prompts = append(s.Prompts, and(e.promptCond, base, vis))
	}
	for _, d := range e.defaults {
		s.Defaults = append(s.Defaults, Default{d.Value, and(d.Cond, base)})
	}
	for _, sel := range e.selects {
		t := p.sym(sel.name)
		t.RevDep = append(t.RevDep, and(symExpr(s.Name), sel.cond, base))
	}
	for _, im := range e.implies {
		t := p.sym(im.name)
		t.Implied = append(t.Implied, and(symExpr(s.Name), im.cond, base))
	}
	if ch := p.choice(); ch != nil {
		if s.Choice == nil {
			s.Choice = ch
			ch.Members = append(ch.Members, s)
		}
		// An untyped choice takes the type of its first member.
		if !ch.typed && len(ch.Members) == 1 {
			ch.Tristate = s.Type == Tristate
		}
		return
	}
	s.DirDep = append(s.DirDep, base)
}

// choice returns the choice the current entries belong to, if any.
func (p *parser) choice() *Choice {
	for i := len(p.blocks) - 1; i >= 0; i-- {
		switch p.blocks[i].kind {
		case "choice":
			return p.blocks[i].choice
		case "menu":
			return nil
		}
	}
	return nil
}
