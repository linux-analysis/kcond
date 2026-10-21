package kcond

import (
	"fmt"
	"strings"
)

// Source describes the lines of a C file: Cpp[i], Guard[i] and Code[i]
// belong to line i+1.
type Source struct {
	// Cpp is the preprocessor condition of each line (see Lines).
	Cpp []*Expr
	// Guard is the condition of the C if statements around each line that
	// test Kconfig symbols with IS_ENABLED(), IS_BUILTIN(), IS_MODULE(),
	// IS_REACHABLE() or config_enabled(): a then branch gets the
	// condition, an else branch its negation. A line holding tokens from
	// several branches gets the OR of their conditions.
	Guard []*Expr
	// Code tells whether a line holds anything but comments and blanks.
	Code []bool
	// GuardErr is set when the statement structure could not be followed,
	// typically a brace opened in two #ifdef branches; every Guard is then
	// true.
	GuardErr error
}

// Analyze computes the preprocessor regions, C guards and code lines of a C
// source, or the #[cfg] regions of a Rust one (see analyzeRust); code is
// as for Lines. It fails only when the preprocessor directives are
// unbalanced.
func Analyze(src []byte, code Code) (*Source, error) {
	if code.Rust {
		return analyzeRust(src), nil
	}
	cpp, err := Lines(src, code)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(src), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	s := &Source{Cpp: cpp, Guard: make([]*Expr, len(lines)), Code: make([]bool, len(lines))}
	toks := tokenize(lines, cpp, s.Code)
	conds, err := guards(toks)
	for i := range s.Guard {
		s.Guard[i] = Const(true)
	}
	if err != nil {
		s.GuardErr = err
		return s, nil
	}
	// A line is reached when any of its tokens is.
	perLine := map[int][]*Expr{}
	for i, t := range toks {
		perLine[t.line] = append(perLine[t.line], conds[i])
	}
	for l, cs := range perLine {
		s.Guard[l] = Or(cs...)
	}
	return s, nil
}

// Cond is the condition under which the code on line index i (0-based) is
// compiled and not discarded by a guard.
func (s *Source) Cond(i int) *Expr { return And(s.Cpp[i], s.Guard[i]) }

// ctoken is a C token and the 0-based line it starts on.
type ctoken struct {
	text string
	line int
}

var cPunct = []string{"<<=", ">>=", "...", "->", "++", "--", "&&", "||", "==", "!=", "<=", ">=",
	"<<", ">>", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "##"}

// tokenize splits the C code of lines into tokens. Comments, string and
// character literals (kept as one token), preprocessor directives and lines
// in regions the preprocessor always drops are skipped. It marks in code the
// lines that hold anything but comments and blanks.
func tokenize(lines []string, cpp []*Expr, code []bool) []ctoken {
	var toks []ctoken
	inComment, directive := false, false
	for li, line := range lines {
		stripped, nextComment := StripComments(line, inComment)
		trimmed := strings.TrimSpace(stripped)
		code[li] = trimmed != ""
		dead := cpp[li].IsConst(false)
		startsDirective := !directive && strings.HasPrefix(trimmed, "#")
		inDirective := directive || startsDirective
		directive = inDirective && strings.HasSuffix(strings.TrimRight(line, " \t\r"), "\\")
		inComment = nextComment
		if inDirective || dead {
			continue
		}
		toks = append(toks, lexLine(stripped, li)...)
	}
	return toks
}

// lexLine tokenizes one line with its comments stripped.
func lexLine(line string, li int) []ctoken {
	var toks []ctoken
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(line) && line[j] != c {
				if line[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j+1, len(line))
			toks = append(toks, ctoken{line[i:j], li})
			i = j
		case isIdentByte(c):
			j := i
			for j < len(line) && isIdentByte(line[j]) {
				j++
			}
			toks = append(toks, ctoken{line[i:j], li})
			i = j
		default:
			n := 1
			for _, p := range cPunct {
				if strings.HasPrefix(line[i:], p) {
					n = len(p)
					break
				}
			}
			toks = append(toks, ctoken{line[i : i+n], li})
			i += n
		}
	}
	return toks
}

// guards returns the guard condition of each token.
func guards(toks []ctoken) ([]*Expr, error) {
	g := &gparser{toks: toks, conds: make([]*Expr, len(toks)), cur: Const(true)}
	for g.pos < len(toks) {
		if err := g.statement(); err != nil {
			return nil, err
		}
	}
	return g.conds, nil
}

// gparser walks the statement skeleton of a C file: blocks, if/else, loops,
// labels and expression statements. It knows nothing else about C.
type gparser struct {
	toks  []ctoken
	conds []*Expr
	pos   int
	cur   *Expr
}

func (g *gparser) peek() string {
	if g.pos < len(g.toks) {
		return g.toks[g.pos].text
	}
	return ""
}

func (g *gparser) take() {
	g.conds[g.pos] = g.cur
	g.pos++
}

func (g *gparser) errorf(format string, args ...any) error {
	line := 0
	if g.pos < len(g.toks) {
		line = g.toks[g.pos].line + 1
	} else if len(g.toks) > 0 {
		line = g.toks[len(g.toks)-1].line + 1
	}
	return fmt.Errorf("kcond: guards: line %d: %s", line, fmt.Sprintf(format, args...))
}

// statement consumes one statement. At file scope it also consumes
// declarations and function definitions, whose bodies are blocks.
func (g *gparser) statement() error {
	switch tok := g.peek(); tok {
	case "":
		return g.errorf("unexpected end of file")
	case "{":
		return g.block()
	case "}":
		return g.errorf("unbalanced }")
	case "if":
		return g.ifStatement()
	case "for", "while", "switch":
		g.take()
		if g.peek() == "(" {
			if err := g.parens(); err != nil {
				return err
			}
		}
		return g.statement()
	case "do":
		g.take()
		if err := g.statement(); err != nil {
			return err
		}
		// The while (...); that ends it, so that an else after it finds
		// its if.
		if g.peek() == "while" {
			return g.simple()
		}
		return nil
	case "else":
		g.take()
		return g.statement()
	case "case", "default":
		for g.peek() != ":" && g.peek() != "" {
			g.take()
		}
		if g.peek() == "" {
			return g.errorf("unterminated %s label", tok)
		}
		g.take()
		return nil
	}
	if g.pos+1 < len(g.toks) && g.toks[g.pos+1].text == ":" && isIdentByte(g.peek()[0]) {
		g.take() // label
		g.take()
		return nil
	}
	return g.simple()
}

// simple consumes an expression statement or declaration: tokens up to a
// ; at depth zero, or a block that ends it (a function body, a struct or
// an initializer).
func (g *gparser) simple() error {
	for {
		switch g.peek() {
		case "":
			return nil
		case ";":
			g.take()
			return nil
		case "(", "[":
			if err := g.parens(); err != nil {
				return err
			}
		case "{":
			if err := g.block(); err != nil {
				return err
			}
			// A function body or struct ends the statement unless a
			// declarator list or ; follows.
			if g.peek() != ";" && g.peek() != "," && g.peek() != "=" && !isDeclaratorStart(g.peek()) {
				return nil
			}
		case "}":
			return nil
		case "if":
			// The body of an iterator macro: for_each_cpu(c, m) if (...).
			return g.statement()
		default:
			g.take()
		}
	}
}

func isDeclaratorStart(tok string) bool {
	return tok == "*" || tok != "" && isIdentByte(tok[0]) && tok != "if" && tok != "else" && tok != "for" &&
		tok != "while" && tok != "do" && tok != "switch" && tok != "return" && tok != "case" && tok != "default"
}

// block consumes { statements }.
func (g *gparser) block() error {
	g.take()
	for g.peek() != "}" {
		if g.peek() == "" {
			return g.errorf("unterminated block")
		}
		if err := g.statement(); err != nil {
			return err
		}
	}
	g.take()
	return nil
}

// parens consumes a balanced ( ) or [ ] group, parsing the blocks of
// statement expressions inside it.
func (g *gparser) parens() error {
	open := g.peek()
	closing := ")"
	if open == "[" {
		closing = "]"
	}
	g.take()
	for g.peek() != closing {
		switch g.peek() {
		case "":
			return g.errorf("unterminated %s", open)
		case "(", "[":
			if err := g.parens(); err != nil {
				return err
			}
		case "{":
			if err := g.block(); err != nil {
				return err
			}
		case ")", "]", "}":
			return g.errorf("unbalanced %s", g.peek())
		default:
			g.take()
		}
	}
	g.take()
	return nil
}

// ifStatement consumes if (cond) stmt [else stmt], guarding the branches
// when cond tests a Kconfig symbol.
func (g *gparser) ifStatement() error {
	g.take()
	if g.peek() != "(" {
		return g.errorf("if without (")
	}
	start := g.pos
	if err := g.parens(); err != nil {
		return err
	}
	// The tokens are cloned: the condition's atoms would otherwise keep
	// the whole source alive.
	var texts []string
	for _, t := range g.toks[start+1 : g.pos-1] {
		texts = append(texts, strings.Clone(t.text))
	}
	cond := guardCond(texts)
	guarded := len(cond.Symbols()) > 0
	outer := g.cur
	if guarded {
		g.cur = And(outer, cond)
	}
	if err := g.statement(); err != nil {
		return err
	}
	g.cur = outer
	if g.peek() != "else" {
		return nil
	}
	g.take()
	if guarded {
		g.cur = And(outer, Not(cond))
	}
	err := g.statement()
	g.cur = outer
	return err
}

// guardCond converts the tokens of an if condition: operands joined by
// &&, || and ! that are guard macros on CONFIG_ symbols become variables,
// any other operand an opaque atom named by its tokens.
func guardCond(toks []string) *Expr {
	p := &guardParser{toks: toks}
	e := p.or()
	if p.pos < len(toks) {
		return Var(strings.Join(toks, " "), Enabled)
	}
	return e
}

type guardParser struct {
	toks []string
	pos  int
}

func (p *guardParser) or() *Expr {
	terms := []*Expr{p.and()}
	for p.pos < len(p.toks) && p.toks[p.pos] == "||" {
		p.pos++
		terms = append(terms, p.and())
	}
	return Or(terms...)
}

func (p *guardParser) and() *Expr {
	terms := []*Expr{p.operand()}
	for p.pos < len(p.toks) && p.toks[p.pos] == "&&" {
		p.pos++
		terms = append(terms, p.operand())
	}
	return And(terms...)
}

var guardMacros = map[string]State{"IS_ENABLED": Enabled, "IS_BUILTIN": Builtin,
	"IS_MODULE": Module, "IS_REACHABLE": Enabled}

// macroCall converts a macro applied to a CONFIG_ symbol, such as
// IS_ENABLED(CONFIG_X); ok is false for macros that test no option.
func macroCall(name, sym string) (e *Expr, ok bool) {
	if st, ok := guardMacros[name]; ok {
		return Var(sym, st), true
	}
	if name == "config_enabled" {
		return defined(sym), true
	}
	return nil, false
}

// operand consumes the tokens up to the next && or || at depth zero.
func (p *guardParser) operand() *Expr {
	start := p.pos
	depth := 0
	for p.pos < len(p.toks) {
		t := p.toks[p.pos]
		if depth == 0 && (t == "&&" || t == "||" || t == ")") {
			break
		}
		switch t {
		case "(", "[":
			depth++
		case ")", "]":
			depth--
		}
		p.pos++
	}
	return operandExpr(p.toks[start:p.pos])
}

func operandExpr(toks []string) *Expr {
	switch {
	case len(toks) == 0:
		return Var("", Enabled)
	case toks[0] == "!":
		return Not(operandExpr(toks[1:]))
	case toks[0] == "(" && closesAtEnd(toks):
		return guardCond(toks[1 : len(toks)-1])
	case len(toks) == 4 && toks[1] == "(" && toks[3] == ")" && IsSymbol(toks[2]):
		if e, ok := macroCall(toks[0], toks[2]); ok {
			return e
		}
	}
	return Var(strings.Join(toks, " "), Enabled)
}

// closesAtEnd reports whether the ( at toks[0] is closed by the last token.
func closesAtEnd(toks []string) bool {
	depth := 0
	for i, t := range toks {
		switch t {
		case "(", "[":
			depth++
		case ")", "]":
			depth--
			if depth == 0 {
				return i == len(toks)-1
			}
		}
	}
	return false
}
